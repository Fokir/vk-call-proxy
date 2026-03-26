# Multi-Call Pool v2 — Design Spec

**Date:** 2026-03-27
**Branch:** feat/rate-limited-allocations
**Status:** Approved

## Goal

N parallel VK calls feeding one shared MUX for fault tolerance and bandwidth aggregation. Each call provides M TURN relay connections. Total: N×M connections in shared MUX.

## Data Flow

```
Client:
  App → SOCKS5/HTTP proxy → shared MUX
    → conn₁ (call₁) → DTLS → TURN_client ↔ TURN_server → DTLS → shared MUX → Internet
    → conn₂ (call₁) → DTLS → TURN_client ↔ TURN_server → DTLS →     ↑
    → conn₃ (call₂) → DTLS → TURN_client ↔ TURN_server → DTLS →     ↑
    → conn₄ (call₂) → DTLS → TURN_client ↔ TURN_server → DTLS →     ↑

Signaling:
  Client WS₁ (call₁) ←→ VK ←→ Server WS₁ (call₁)
  Client WS₂ (call₂) ←→ VK ←→ Server WS₂ (call₂)
  Critical messages broadcast through ALL websockets, dedup by nonce.
```

Example: `--link=A --link=B --n=2` → 2 calls × 2 conns = 4 connections in shared MUX.

## Package Structure

New package `internal/tunnel/` — provider-agnostic orchestration layer.

```
internal/tunnel/
  pool.go       — CallPool: manages N CallSlots + shared MUX
  slot.go       — CallSlot: one call lifecycle (join → relay → DTLS → monitor → reconnect)
  signaling.go  — SignalingRouter: multi-WS broadcast + dedup by nonce
  config.go     — PoolConfig, SlotConfig, SlotStatus, constants
```

### pool.go — CallPool

```go
type CallPool struct {
    slots   []*CallSlot
    mux     *mux.Mux
    router  *SignalingRouter
    config  PoolConfig
    logger  *slog.Logger
}

type PoolConfig struct {
    Links        []string
    ConnsPerCall int              // --n flag (default 4)
    UseTCP       bool
    AuthToken    string           // VPN auth token (shared across all calls)
    VKTokens     []string
    Services     []provider.Service  // one per link, created externally
}
```

**Lifecycle:**
1. `NewCallPool(cfg)` — creates pool with empty MUX
2. `Start(ctx)` — sequential slot connect (first ready → return MUX for proxy), remaining in background
3. `Mux()` — returns shared MUX
4. `Status() []SlotStatus` — health of all slots
5. `Close()` — graceful shutdown

### slot.go — CallSlot

```go
type CallSlot struct {
    index       int
    link        string
    svc         provider.Service
    sigClient   provider.SignalingClient
    turnMgr     *turn.Manager
    connIndexes []int              // indices in shared MUX
    status      SlotStatus
}
```

**Lifecycle:**
1. `Connect(ctx)` — FetchJoinInfo → ConnectSignaling → register in router
2. `AllocateConns(ctx, n)` — TURN allocate × n → DTLS → AddConn to shared MUX
3. `Monitor(ctx)` — watches conn health + signaling health
4. `Reconnect(ctx)` — full reconnect with backoff (3s → 60s cap, infinite retries)

### signaling.go — SignalingRouter

```go
type SignalingRouter struct {
    clients []provider.SignalingClient
    seen    sync.Map  // nonce → struct{} for dedup
}
```

**API:**
- `Register(sigClient)` — adds WS channel
- `Remove(sigClient)` — removes dead WS
- `Broadcast(msg)` — sends through ALL websockets
- `Receive(ctx) (msg, error)` — listens on all WS, returns first unique message (dedup by nonce)

Dedup key: `nonce + callIndex` — prevents processing same message received from multiple websockets.

## Connection Sequence

### Client Start

```
1. For i in [0, N):        // sequential, rate limit safe
     if i > 0: wait 3s
     slot[i].Connect()     // FetchJoinInfo + ConnectSignaling
     router.Register(slot[i].sigClient)

2. slot[0].AllocateConns() // first slot allocates all conns
   → MUX created with first conn
   → proxy starts (SOCKS5 + HTTP)
   → remaining conns added to MUX

3. Background: for i in [1, N):
     slot[i].AllocateConns()  // remaining slots add conns to shared MUX
```

### Server Start

```
1. For i in [0, N):        // sequential
     if i > 0: wait 3s
     slot[i].Connect()     // FetchJoinInfo + ConnectSignaling
     router.Register(slot[i].sigClient)
     slot[i].PreAllocate(1) // 1 TURN allocation per slot (fast client connect)

2. Wait: router.Receive()  // wait for client relay addrs (any WS)

3. Exchange: broadcast server pre-allocated relay addrs (instant, no allocation delay)

4. DTLS handshake on pre-allocated conns
   → MUX created → hybrid mode (streams + raw packets)

5. Background: allocate remaining conns per slot (--n minus 1)
```

### Relay Address Exchange (Broadcast + Dedup)

```
Client sends relay addrs for call₁:
  → broadcast through WS₁ AND WS₂ (all websockets)
  → each message has: nonce, callIndex, relay addrs, role

Server receives:
  → WS₁ delivers message (nonce=X, callIndex=0)  → PROCESS (first seen)
  → WS₂ delivers message (nonce=X, callIndex=0)  → SKIP (dedup: nonce+callIndex already seen)
  → responds with server relay addrs via broadcast
```

## Reconnect Strategy

### Principle: all VK API calls go through serial reconnect queue

Never parallel VK API requests from reconnect — risk of rate limit ban.

### Reconnect Queue

```
reconnectQueue (serial, one at a time):
  slot₃ needs reconnect → push to queue
  slot₁ needs reconnect → push to queue

  queue processes:
    slot₃: FetchJoinInfo → ConnectSignaling → AllocateConns → done
    wait 3s (rate limit safety)
    slot₁: FetchJoinInfo → ConnectSignaling → AllocateConns → done
```

### Failure Levels

| Failure | Detection | Reaction | Impact |
|---------|-----------|----------|--------|
| 1 conn dies | readLoop exit → connDied | Re-allocate 1 TURN conn in slot | None (MUX continues on other conns) |
| All conns in slot | All connIndexes removed | Full slot reconnect via queue | Temporary loss of M conns |
| Signaling WS dies | WS read error | WS reconnect, router.Remove() | None (TURN alive without WS) |
| All slots die | MUX allDead | All slots queued for serial reconnect, first ready → restore | Brief outage until first slot reconnects |

### Backoff

- Single conn re-alloc: 3s fixed delay
- Full slot reconnect: linear backoff 3s, 6s, 9s... cap 60s
- Reset backoff on successful reconnect
- Infinite retries (call links are permanent)

## Graceful Degradation

- Client has 2 links, server has 4 → only 2 common calls active, server's 2 extra slots idle
- Client has 4 links, server has 2 → only 2 common calls active, client's 2 extra slots idle
- No error, no failure — slots without peers simply wait
- Single `--link` → no pool, current behavior (backward compatible)

## CLI Interface

```bash
# Single call (backward compatible, no pool)
--link=ABC --n=4

# Multi-call pool
--link=ABC --link=DEF --link=GHI --n=4    # 3 calls × 4 conns = 12 total

# Both sides
server --link=ABC --link=DEF --n=4 --token=secret
client --link=ABC --link=DEF --n=4 --token=secret --vk-token=TOKEN
```

`--link` is repeatable. `--n` applies per call (same for all calls).

## UI (server-ui, mobile)

- Profile editor: up to 8 call links
- "+ добавить" button next to last link input
- Links stored in profile config

## Provider Agnosticism

`CallPool` accepts `[]provider.Service` — one per link. It does not know about VK, Telemost, or any specific provider. Provider-specific logic (auth chain, signaling protocol) stays in `internal/provider/*`.

Future: mix providers in one pool (e.g., 2 VK calls + 1 Telemost call).

## What Changes in Existing Code

| File | Change |
|------|--------|
| `cmd/client/main.go` | `--link` repeatable flag, create pool if len(links) > 1 |
| `cmd/server/main.go` | Same |
| `internal/client/client.go` | `RunRelayToRelay` routes to pool.Start() if multi-link |
| `internal/server/server.go` | `runRelayMode` routes to pool if multi-link |
| `internal/mux/mux.go` | trySendInFrame panic recovery (AddConn/Close race fix) |
| `cmd/server-ui/main.go` | Multi-link profile field |
| `mobile/bind/tunnel.go` | Multi-link support |

## What Does NOT Change

- MUX protocol (frame format, striping, reorder buffer)
- DTLS handshake
- TURN allocation flow
- Proxy layer (SOCKS5, HTTP)
- Provider interfaces
- Auth token / session ID protocol

## Testing Plan

1. Unit: SignalingRouter broadcast + dedup
2. Unit: CallSlot lifecycle (connect → allocate → monitor)
3. Unit: Reconnect queue serialization
4. Integration: 2 calls × 2 conns, verify shared MUX works
5. E2E: `--link=A --link=B --n=2`, speed test, kill one call, verify tunnel continues
6. E2E: graceful degradation (client 1 link, server 2 links)
7. E2E: reconnect after slot death

## test-e2e.sh Adaptation

Current `test-e2e.sh` supports single `--link`. Must be extended for multi-call testing.

### New parameters

```bash
# Single call (current behavior)
./test-e2e.sh --n=4

# Multi-call pool
./test-e2e.sh --n=2 --links=2

# Custom: 3 calls × 4 conns, with monitoring
./test-e2e.sh --n=4 --links=3 --monitor=5
```

- `--links=N` — number of call links to use from .env (default: 1)
- Requires N call links in .env: `VK_CALL_LINK_1`, `VK_CALL_LINK_2`, ... or comma-separated `VK_CALL_LINKS=link1,link2,link3`

### .env format extension

```bash
# Single call (backward compatible)
VK_CALL_LINK=fnKPjrFaF...

# Multi-call (new)
VK_CALL_LINK_1=fnKPjrFaF...
VK_CALL_LINK_2=abcdef123...
VK_CALL_LINK_3=xyz789abc...
```

### Script changes

1. Parse `--links=N`, read N links from .env
2. Build server/client command with multiple `--link=` flags
3. Speed test: compare single-call vs multi-call throughput
4. Fault tolerance test: kill one call (how? — TBD: maybe close browser tab for one call), verify tunnel continues
5. Report per-slot status if available (server/client expose status endpoint or log)

### Test scenarios

```bash
# Baseline: 1 call × 4 conns
./test-e2e.sh --n=4 --links=1 --download-url=http://speedtest.selectel.ru/10MB

# Multi-call: 2 calls × 2 conns (same total: 4)
./test-e2e.sh --n=2 --links=2 --download-url=http://speedtest.selectel.ru/10MB

# Multi-call: 2 calls × 4 conns (8 total)
./test-e2e.sh --n=4 --links=2 --download-url=http://speedtest.selectel.ru/10MB

# Stability: 2 calls × 2 conns, 10 min monitoring
./test-e2e.sh --n=2 --links=2 --monitor=10
```

## Acceptance Criteria (post-implementation E2E testing)

Implementation is complete only after ALL of these pass:

### 1. Basic connectivity
- [ ] `--link=A --n=4` (single call) works identically to current behavior
- [ ] `--link=A --link=B --n=2` (multi-call) establishes tunnel, proxy responds
- [ ] HTTP and SOCKS5 proxies both work through multi-call tunnel

### 2. Speed
- [ ] Multi-call (2×2=4 conns) throughput ≥ single-call (1×4=4 conns) on selectel 10MB
- [ ] No regression: single-call speed unchanged vs current baseline (~100 KB/s)

### 3. Stability
- [ ] 10 minute monitoring with `--links=2 --n=2 --monitor=10`: all connectivity checks pass
- [ ] No reconnect storms (serial queue works, no rate limit errors in logs)
- [ ] Server pre-allocation works: client connect time with multi-call ≤ single-call + 5s

### 4. Fault tolerance
- [ ] Kill one call's browser tab: tunnel continues on remaining call(s)
- [ ] Killed call's slot reconnects automatically (verify in logs)
- [ ] After reconnect: all conns restored, speed back to normal

### 5. Graceful degradation
- [ ] Client 1 link + server 2 links: tunnel works on 1 common call
- [ ] Client 2 links + server 1 link: tunnel works on 1 common call
- [ ] Mismatched links: no errors, idle slots visible in logs

### 6. Signaling redundancy
- [ ] Relay addr exchange succeeds even if sent through non-primary WS
- [ ] Duplicate messages are filtered (no double allocation or double handshake)

### 7. Build & unit tests
- [ ] `go build ./...` clean
- [ ] `go test ./...` all pass
- [ ] `go test ./internal/tunnel/...` new tests pass
