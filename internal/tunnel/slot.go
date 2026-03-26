package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/turn"
)

// Role determines client vs server behavior for DTLS handshake direction.
type Role int

const (
	RoleClient Role = iota
	RoleServer
)

// CallSlot manages one call's lifecycle within the pool.
type CallSlot struct {
	index     int
	svc       provider.Service
	role      Role
	useTCP    bool
	authToken string
	vkTokens  []string
	fp        []byte // server cert fingerprint (client only)
	logger    *slog.Logger

	mu        sync.Mutex
	state     SlotState
	sigClient provider.SignalingClient
	mgr       *turn.Manager
	nonce     string
	conns     []net.Conn
	cleanups  []context.CancelFunc
	lastErr   error
	activeC   atomic.Int32
}

// NewCallSlot creates a new slot for the given call.
func NewCallSlot(index int, svc provider.Service, role Role, useTCP bool, authToken string, vkTokens []string, fp []byte, logger *slog.Logger) *CallSlot {
	return &CallSlot{
		index:     index,
		svc:       svc,
		role:      role,
		useTCP:    useTCP,
		authToken: authToken,
		vkTokens:  vkTokens,
		fp:        fp,
		logger:    logger.With("slot", index, "link", svc.Name()),
		state:     SlotIdle,
	}
}

func generateNonce() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// State returns the current slot state.
func (s *CallSlot) State() SlotState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Nonce returns this slot's session nonce.
func (s *CallSlot) Nonce() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nonce
}

// SignalingClient returns the slot's signaling client (nil if not connected).
func (s *CallSlot) SignalingClient() provider.SignalingClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sigClient
}

// Connect joins the call and establishes signaling.
func (s *CallSlot) Connect(ctx context.Context) error {
	s.mu.Lock()
	s.state = SlotConnecting
	s.mu.Unlock()

	s.logger.Info("connecting to call")

	info, err := s.svc.FetchJoinInfo(ctx)
	if err != nil {
		s.setError(err)
		return fmt.Errorf("slot %d: fetch join info: %w", s.index, err)
	}

	sigClient, err := s.svc.ConnectSignaling(ctx, info, s.logger)
	if err != nil {
		s.setError(err)
		return fmt.Errorf("slot %d: connect signaling: %w", s.index, err)
	}

	if s.authToken != "" {
		if err := sigClient.SetKey(s.authToken); err != nil {
			sigClient.Close()
			s.setError(err)
			return fmt.Errorf("slot %d: set key: %w", s.index, err)
		}
	}

	mgr := turn.NewManager(s.svc, s.useTCP, s.logger)
	nonce := generateNonce()

	s.mu.Lock()
	s.sigClient = sigClient
	s.mgr = mgr
	s.nonce = nonce
	s.mu.Unlock()

	s.logger.Info("connected to call", "nonce", nonce)
	return nil
}

// PreAllocate allocates n TURN connections ahead of time (server mode).
// Returns the allocations for later use in DTLS handshake.
func (s *CallSlot) PreAllocate(ctx context.Context, n int) ([]*turn.Allocation, error) {
	s.mu.Lock()
	mgr := s.mgr
	s.mu.Unlock()

	if mgr == nil {
		return nil, fmt.Errorf("slot %d: not connected", s.index)
	}

	allocs, err := mgr.Allocate(ctx, n)
	if err != nil {
		s.setError(err)
		return nil, fmt.Errorf("slot %d: pre-allocate: %w", s.index, err)
	}

	s.logger.Info("pre-allocated TURN", "count", len(allocs))
	return allocs, nil
}

// AllocateAndConnect allocates TURN, exchanges relay addresses via the router,
// performs DTLS handshake, and adds connections to the shared MUX.
// Returns the MUX (created on first conn if m is nil).
func (s *CallSlot) AllocateAndConnect(ctx context.Context, m *mux.Mux, router *SignalingRouter, n int, sessionID [16]byte) (*mux.Mux, error) {
	s.mu.Lock()
	s.state = SlotAllocating
	sigClient := s.sigClient
	mgr := s.mgr
	nonce := s.nonce
	s.mu.Unlock()

	if sigClient == nil || mgr == nil {
		return m, fmt.Errorf("slot %d: not connected", s.index)
	}

	return s.allocateClient(ctx, m, router, mgr, sigClient, nonce, n, sessionID)
}

// AllocateAndConnectServer is the server-side entry point.
// preAllocs are TURN allocations from PreAllocate() — used for instant first-batch response.
func (s *CallSlot) AllocateAndConnectServer(ctx context.Context, m *mux.Mux, router *SignalingRouter, n int, preAllocs []*turn.Allocation) (*mux.Mux, error) {
	s.mu.Lock()
	s.state = SlotAllocating
	sigClient := s.sigClient
	mgr := s.mgr
	nonce := s.nonce
	s.mu.Unlock()

	if sigClient == nil || mgr == nil {
		return m, fmt.Errorf("slot %d: not connected", s.index)
	}

	return s.allocateServer(ctx, m, router, mgr, sigClient, nonce, n, preAllocs)
}

func (s *CallSlot) allocateClient(ctx context.Context, m *mux.Mux, router *SignalingRouter, mgr *turn.Manager, sigClient provider.SignalingClient, nonce string, n int, sessionID [16]byte) (*mux.Mux, error) {
	// Disconnect handshake (clear previous session)
	dctx, dcancel := context.WithTimeout(ctx, 6*time.Second)
	sigClient.SendDisconnectReq(dctx, nonce)
	sigClient.WaitDisconnectAck(dctx, nonce)
	dcancel()

	sigClient.StartPunchDispatcher(ctx, nonce)
	defer sigClient.StopPunchDispatcher()

	// Phase 1: Token allocations (parallel, fast)
	tokens := deduplicateTokens(s.vkTokens)
	tokenCount := min(len(tokens), n)
	var tokenAllocs []*turn.Allocation
	if tokenCount > 0 {
		s.logger.Info("allocating with tokens", "count", tokenCount)
		tokenAllocs = allocateWithTokens(ctx, s.svc, mgr, tokens[:tokenCount], s.logger)
	}

	// Phase 2: Anonymous allocations (gradual, batched)
	anonCount := n - len(tokenAllocs)
	var anonBatches <-chan turn.BatchResult
	if anonCount > 0 {
		s.logger.Info("allocating anonymous", "count", anonCount)
		anonBatches = mgr.AllocateGradual(ctx, anonCount, turn.GradualOpts{})
	}

	// Exchange + DTLS for token allocations (batch 0)
	punchIdx := 0
	if len(tokenAllocs) > 0 {
		addrs := allocAddrs(tokenAllocs)
		if err := router.BroadcastRelayBatch(ctx, addrs, "client", nonce, 0, anonCount == 0); err != nil {
			return m, fmt.Errorf("slot %d: broadcast batch 0: %w", s.index, err)
		}

		serverAddrs, _, _, _, err := sigClient.RecvRelayBatch(ctx, "client", nonce)
		if err != nil {
			return m, fmt.Errorf("slot %d: recv batch 0: %w", s.index, err)
		}

		m, err = s.dtlsHandshakeParallel(ctx, m, tokenAllocs, serverAddrs, sigClient, nonce, punchIdx, sessionID)
		if err != nil {
			return m, err
		}
		punchIdx += len(tokenAllocs)
	}

	// Exchange + DTLS for anonymous batches
	if anonBatches != nil {
		batchNum := 1
		if len(tokenAllocs) == 0 {
			batchNum = 0
		}
		for br := range anonBatches {
			if len(br.Allocs) == 0 {
				if br.Final {
					break
				}
				continue
			}

			addrs := allocAddrs(br.Allocs)
			if err := router.BroadcastRelayBatch(ctx, addrs, "client", nonce, batchNum, br.Final); err != nil {
				s.logger.Warn("broadcast batch failed", "batch", batchNum, "err", err)
				continue
			}

			serverAddrs, _, _, _, err := sigClient.RecvRelayBatch(ctx, "client", nonce)
			if err != nil {
				s.logger.Warn("recv batch failed", "batch", batchNum, "err", err)
				continue
			}

			m, err = s.dtlsHandshakeParallel(ctx, m, br.Allocs, serverAddrs, sigClient, nonce, punchIdx, sessionID)
			if err != nil {
				s.logger.Warn("dtls batch failed", "batch", batchNum, "err", err)
			}
			punchIdx += len(br.Allocs)
			batchNum++

			if br.Final {
				break
			}
		}
	}

	if m == nil {
		return nil, fmt.Errorf("slot %d: no connections established", s.index)
	}

	s.mu.Lock()
	s.state = SlotActive
	s.mu.Unlock()
	s.logger.Info("slot active", "conns", s.activeC.Load())
	return m, nil
}

func (s *CallSlot) allocateServer(ctx context.Context, m *mux.Mux, router *SignalingRouter, mgr *turn.Manager, sigClient provider.SignalingClient, _ string, _ int, preAllocs []*turn.Allocation) (*mux.Mux, error) {
	tokenIdx := 0
	tokens := deduplicateTokens(s.vkTokens)
	punchIdx := 0
	usedPreAlloc := false
	clientNonce := "" // learned from first client message (server doesn't know client's nonce upfront)

	for batch := 0; ; batch++ {
		// First receive uses no filter (clientNonce="") to learn client's nonce.
		// Subsequent receives filter by learned client nonce.
		clientAddrs, _, final, recvNonce, err := sigClient.RecvRelayBatch(ctx, "server", clientNonce)
		if err != nil {
			return m, fmt.Errorf("slot %d: recv client batch %d: %w", s.index, batch, err)
		}
		if clientNonce == "" {
			clientNonce = recvNonce
			s.logger.Info("learned client nonce", "nonce", clientNonce)
		}

		needed := len(clientAddrs)
		var allocs []*turn.Allocation

		// First batch: use pre-allocated TURN if available
		if !usedPreAlloc && len(preAllocs) > 0 {
			usedPreAlloc = true
			preCount := min(len(preAllocs), needed)
			allocs = append(allocs, preAllocs[:preCount]...)
			remaining := needed - preCount
			if remaining > 0 {
				extra := s.allocateMatching(ctx, mgr, remaining, tokens, &tokenIdx)
				allocs = append(allocs, extra...)
			}
		} else {
			allocs = s.allocateMatching(ctx, mgr, needed, tokens, &tokenIdx)
		}

		if len(allocs) == 0 {
			if final {
				break
			}
			continue
		}

		// Respond with CLIENT's nonce (not server's) so client can filter by its own nonce
		serverAddrs := allocAddrs(allocs)
		if err := router.BroadcastRelayBatch(ctx, serverAddrs, "server", clientNonce, batch, final); err != nil {
			s.logger.Warn("broadcast server addrs failed", "err", err)
		}

		m, err = s.dtlsHandshakeParallel(ctx, m, allocs, clientAddrs, sigClient, clientNonce, punchIdx, [16]byte{})
		if err != nil {
			s.logger.Warn("dtls batch failed", "batch", batch, "err", err)
		}
		punchIdx += len(allocs)

		if final {
			break
		}
	}

	if m == nil {
		return nil, fmt.Errorf("slot %d: no connections established", s.index)
	}

	s.mu.Lock()
	s.state = SlotActive
	s.mu.Unlock()
	return m, nil
}

func (s *CallSlot) allocateMatching(ctx context.Context, mgr *turn.Manager, needed int, tokens []string, tokenIdx *int) []*turn.Allocation {
	var allocs []*turn.Allocation
	for i := 0; i < needed && *tokenIdx < len(tokens); i++ {
		a, err := allocateWithToken(ctx, s.svc, mgr, tokens[*tokenIdx])
		*tokenIdx++
		if err != nil {
			s.logger.Warn("token alloc failed", "err", err)
			continue
		}
		allocs = append(allocs, a)
	}
	anonNeeded := needed - len(allocs)
	if anonNeeded > 0 {
		anonAllocs, err := mgr.Allocate(ctx, anonNeeded)
		if err != nil {
			s.logger.Warn("anon alloc failed", "err", err)
		} else {
			allocs = append(allocs, anonAllocs...)
		}
	}
	return allocs
}

// dtlsHandshakeParallel performs DTLS handshake for each (local alloc, remote addr) pair.
// Creates MUX on first successful connection if m is nil.
func (s *CallSlot) dtlsHandshakeParallel(ctx context.Context, m *mux.Mux, allocs []*turn.Allocation, remoteAddrs []string, sigClient provider.SignalingClient, nonce string, punchStartIdx int, sessionID [16]byte) (*mux.Mux, error) {
	type dtlsResult struct {
		conn    net.Conn
		cleanup context.CancelFunc
		idx     int
		err     error
	}

	count := min(len(allocs), len(remoteAddrs))
	results := make(chan dtlsResult, count)

	for i := range count {
		go func(i int) {
			alloc := allocs[i]
			rAddr, err := net.ResolveUDPAddr("udp", remoteAddrs[i])
			if err != nil {
				results <- dtlsResult{idx: i, err: err}
				return
			}

			internaldtls.PunchRelay(alloc.RelayConn, rAddr)
			punchCtx, punchCancel := context.WithCancel(ctx)
			internaldtls.StartPunchLoop(punchCtx, alloc.RelayConn, rAddr)

			pIdx := punchStartIdx + i
			waiter := sigClient.PreparePunchWait(ctx, nonce, pIdx)
			sigClient.SendPunchReady(ctx, nonce, pIdx)
			if err := waiter(); err != nil {
				punchCancel()
				results <- dtlsResult{idx: i, err: fmt.Errorf("punch wait: %w", err)}
				return
			}

			var conn net.Conn
			var cleanup context.CancelFunc
			if s.role == RoleClient {
				conn, cleanup, err = internaldtls.DialOverTURN(ctx, alloc.RelayConn, rAddr, s.fp)
			} else {
				conn, cleanup, err = internaldtls.AcceptOverTURN(ctx, alloc.RelayConn, rAddr)
			}
			punchCancel()

			if err != nil {
				results <- dtlsResult{idx: i, err: fmt.Errorf("dtls: %w", err)}
				return
			}
			results <- dtlsResult{conn: conn, cleanup: cleanup, idx: i}
		}(i)
	}

	var firstErr error
	for range count {
		res := <-results
		if res.err != nil {
			s.logger.Warn("dtls handshake failed", "idx", res.idx, "err", res.err)
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}

		conn := res.conn

		// Client: write auth token + session ID
		if s.role == RoleClient {
			if s.authToken != "" {
				if err := mux.WriteAuthToken(conn, s.authToken); err != nil {
					s.logger.Warn("write auth token failed", "err", err)
					conn.Close()
					if res.cleanup != nil {
						res.cleanup()
					}
					continue
				}
			}
			if err := mux.WriteSessionID(conn, sessionID); err != nil {
				s.logger.Warn("write session id failed", "err", err)
				conn.Close()
				if res.cleanup != nil {
					res.cleanup()
				}
				continue
			}
		}

		// Server: validate auth token + read session ID
		if s.role == RoleServer {
			if s.authToken != "" {
				if err := mux.ValidateAuthToken(conn, s.authToken); err != nil {
					s.logger.Warn("validate auth token failed", "err", err)
					conn.Close()
					if res.cleanup != nil {
						res.cleanup()
					}
					continue
				}
			}
			if _, err := mux.ReadSessionID(conn); err != nil {
				s.logger.Warn("read session id failed", "err", err)
				conn.Close()
				if res.cleanup != nil {
					res.cleanup()
				}
				continue
			}
		}

		// Add to MUX
		if m == nil {
			m = mux.New(s.logger, conn)
		} else {
			m.AddConn(conn)
		}

		s.mu.Lock()
		s.conns = append(s.conns, conn)
		if res.cleanup != nil {
			s.cleanups = append(s.cleanups, res.cleanup)
		}
		s.mu.Unlock()
		s.activeC.Add(1)
	}

	if m == nil && firstErr != nil {
		return nil, firstErr
	}
	return m, nil
}

// Close shuts down this slot's resources.
func (s *CallSlot) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = SlotDead
	if s.sigClient != nil {
		s.sigClient.Close()
		s.sigClient = nil
	}
	if s.mgr != nil {
		s.mgr.CloseAll()
		s.mgr = nil
	}
	for _, cleanup := range s.cleanups {
		cleanup()
	}
	s.cleanups = nil
}

// Status returns a snapshot of this slot's status.
func (s *CallSlot) Status() SlotStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SlotStatus{
		Index:      s.index,
		State:      s.state,
		ActiveConn: int(s.activeC.Load()),
		TotalConn:  len(s.conns),
		LastError:  s.lastErr,
		Link:       s.svc.Name(),
	}
}

func (s *CallSlot) setError(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}

// --- helpers ---

func deduplicateTokens(tokens []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, t := range tokens {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		result = append(result, t)
	}
	return result
}

func allocateWithToken(ctx context.Context, svc provider.Service, mgr *turn.Manager, token string) (*turn.Allocation, error) {
	tap, ok := svc.(provider.TokenAuthProvider)
	if !ok {
		return nil, fmt.Errorf("provider does not support token auth")
	}
	info, err := tap.FetchJoinInfoWithToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("fetch join info with token: %w", err)
	}
	alloc, err := mgr.AllocateWithCredentials(ctx, &info.Credentials)
	if err != nil {
		return nil, fmt.Errorf("allocate with credentials: %w", err)
	}
	return alloc, nil
}

func allocateWithTokens(ctx context.Context, svc provider.Service, mgr *turn.Manager, tokens []string, logger *slog.Logger) []*turn.Allocation {
	type result struct {
		alloc *turn.Allocation
		err   error
	}
	ch := make(chan result, len(tokens))
	for _, t := range tokens {
		go func(token string) {
			a, err := allocateWithToken(ctx, svc, mgr, token)
			ch <- result{a, err}
		}(t)
	}
	var allocs []*turn.Allocation
	for range tokens {
		r := <-ch
		if r.err != nil {
			logger.Warn("token allocation failed", "err", r.err)
			continue
		}
		allocs = append(allocs, r.alloc)
	}
	return allocs
}

func allocAddrs(allocs []*turn.Allocation) []string {
	addrs := make([]string, len(allocs))
	for i, a := range allocs {
		addrs[i] = a.RelayAddr.String()
	}
	return addrs
}
