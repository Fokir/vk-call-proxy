# LuaSolver: Hot-Updatable Captcha Solving Engine

**Date**: 2026-04-17  
**Status**: Approved

## Problem

DirectSolver's captcha-solving logic is compiled into the Go binary. When VK changes their captchaNotRobot protocol, API parameters, or introduces a new captcha type, we must rebuild and redeploy all binaries (server, client, Android APK). This is slow and error-prone.

## Solution

Replace DirectSolver with **LuaSolver** — a thin Go wrapper around an embedded Lua VM (`gopher-lua`). The solving algorithm lives in `solver.lua`, delivered via the existing hot-bundle system. Go provides a rich API surface (HTTP, image processing, crypto, fingerprinting) that Lua scripts call. When VK changes anything, we update `solver.lua` and push — all running instances pick up the change within an hour (or on next restart).

For compute-heavy operations that Lua handles too slowly, Go-native functions are registered under a `native.*` namespace. Lua scripts can call them when available and fall back to pure-Lua implementations when not.

## Architecture

```
┌──────────────────────────────────────────────────┐
│                   ChainSolver                     │
│  ┌─────────┐   ┌──────────┐   ┌───────────────┐ │
│  │LuaSolver│ → │ fallback  │ → │  fallback #2  │ │
│  │(primary)│   │(WebView / │   │ (Interactive / │ │
│  │         │   │ Chromedp) │   │  Remote)       │ │
│  └────┬────┘   └──────────┘   └───────────────┘ │
└───────┼──────────────────────────────────────────┘
        │
        ▼
┌───────────────────┐     ┌─────────────────────┐
│   gopher-lua VM   │────→│   solver.lua         │
│                   │     │   (from hot-bundle)   │
│  Registered mods: │     └─────────────────────┘
│  • http           │
│  • img            │
│  • crypto         │
│  • fp             │
│  • json           │
│  • re             │
│  • url            │
│  • time           │
│  • log            │
│  • native         │
│  • config         │
└───────────────────┘
```

### Component Responsibilities

**LuaSolver** (`internal/captcha/lua.go`):
- Implements `provider.CaptchaSolver` interface
- Creates fresh Lua VM per solve (isolation, no state leakage)
- Loads `solver.lua` from `scripts.Manager` (hot-bundle)
- Registers all Go-backed API modules into the VM
- Calls `solve(challenge)` in Lua, expects `success_token` string return
- Enforces timeout (30s default, configurable)
- If `solver.lua` is missing from bundle, returns error immediately (ChainSolver proceeds to fallback)

**solver.lua** (`internal/scripts/bundled/solver.lua` + `hot-scripts/solver.lua`):
- Contains the complete captcha-solving flow
- Reads config from `config` global (populated from `vk-config.json`)
- Returns `success_token` on success, raises error on failure
- Can be updated via hot-bundle without rebuilding

**API Modules** (`internal/captcha/luamod/`):
- Each module is a separate Go file: `mod_http.go`, `mod_img.go`, `mod_crypto.go`, etc.
- Registered into Lua VM as `http.*`, `img.*`, `crypto.*`, etc.
- Pure Go implementations — no CGO, works on Android

### Integration Points

LuaSolver replaces DirectSolver as the primary solver in all ChainSolver configurations:

| Binary | Chain |
|---|---|
| cmd/server | `LuaSolver → RemoteSolver` |
| cmd/client (with endpoint) | `LuaSolver → RemoteSolver` |
| cmd/client (no endpoint) | `LuaSolver → InteractiveSolver` |
| cmd/captcha-service | `LuaSolver → ChromedpSolver` |
| mobile/bind (Android) | `LuaSolver → callbackSolver (WebView)` |

DirectSolver is kept as a fallback reference implementation but removed from default chains.

## Lua API Specification

### http — Network Requests

All requests share a cookie jar scoped to the current solve session.

```lua
-- GET request
local resp = http.get(url, {["User-Agent"] = "...", ["Accept"] = "..."})
-- resp.status (int), resp.body (string), resp.headers (table)

-- POST with raw body
local resp = http.post(url, headers, body_string)

-- POST form-encoded
local resp = http.post_form(url, headers, {field1 = "val1", field2 = "val2"})

-- POST JSON
local resp = http.post_json(url, headers, {key = "value"})
```

Timeout: inherits from LuaSolver context (30s). No proxy support in Tier 1.

### img — Image Processing

Images are opaque Go objects passed as Lua userdata.

```lua
local image = img.load(bytes)              -- from raw bytes (PNG/JPEG/WebP)
local image = img.decode_base64(str)       -- from base64 data URI
local w = img.width(image)
local h = img.height(image)
local r, g, b, a = img.pixel(image, x, y)

-- Transforms
local cropped = img.crop(image, x, y, w, h)
local resized = img.resize(image, w, h)
local gray = img.grayscale(image)
local edges = img.edge_detect(image)       -- Canny edge detection
local diff = img.diff(img1, img2)          -- pixel-wise difference
local rotated = img.rotate(image, degrees)
local flipped = img.flip(image, true)      -- horizontal flip
local thresh = img.threshold(image, value) -- binarize

-- Analysis
local x, y, score = img.template_match(haystack, needle)
local similarity = img.ssim(img1, img2)
local hist = img.histogram(image)          -- {r={...}, g={...}, b={...}}
local count = img.color_count(image, r, g, b, tolerance)
local colors = img.dominant_colors(image, n) -- [{r,g,b,pct}]

-- Output
local bytes = img.encode(image, "png")
```

### crypto — Cryptography

```lua
local hex = crypto.sha256(data)
local hex = crypto.sha1(data)
local hex = crypto.md5(data)
local hex = crypto.hmac_sha256(key, data)
local hex = crypto.hmac_sha1(key, data)

local str = crypto.base64_encode(data)
local data = crypto.base64_decode(str)

local bytes = crypto.aes_encrypt(key, iv, plaintext)   -- AES-128-CBC
local plaintext = crypto.aes_decrypt(key, iv, ciphertext)

local bytes = crypto.random_bytes(n)
local nonce = crypto.pow_solve(prefix, difficulty)  -- SHA-256 leading zeros
local result = crypto.xor(data1, data2)
```

### fp — Fingerprint & Telemetry Generation

```lua
-- Mouse movement: Bezier curve with realistic jitter
local points = fp.mouse_path(x1, y1, x2, y2, num_points)
-- returns [{x=.., y=..}, ...]

-- Device fingerprint
local device = fp.device_info(seed)
-- returns {screenWidth, screenHeight, language, languages, hardwareConcurrency, ...}

-- Browser fingerprint hash
local hash = fp.browser_fp(seed)

-- Connection telemetry
local rtt = fp.connection_rtt(count)       -- [int, int, ...]
local downlink = fp.connection_downlink(count) -- [float, float, ...]
```

### json, re, url, time, log — Utilities

```lua
-- JSON
local table = json.decode(str)
local str = json.encode(table)

-- Regex (Go regexp syntax)
local captures = re.match(pattern, str)       -- first match, nil if none
local all = re.find_all(pattern, str)         -- all matches
local replaced = re.replace(pattern, str, repl)

-- URL
local encoded = url.encode(str)
local decoded = url.decode(str)
local parts = url.parse(str)  -- {scheme, host, path, query, fragment}

-- Time
time.sleep(milliseconds)
local ms = time.now()  -- Unix milliseconds

-- Logging (goes to Go slog)
log.info("message", "key", "value")
log.debug("message")
log.warn("message", "err", err_string)
```

### config — Hot-Reload Config Access

```lua
-- Populated from vk-config.json before solve() is called
local ua = config.captcha.direct_solver.user_agent
local api_ver = config.captcha.api_version
local answer = config.captcha.checkbox_answer
```

Read-only table, refreshed from `scripts.Manager` on each solve.

### native — Go-Optimized Functions

Optional functions for compute-heavy operations. Lua scripts should check availability:

```lua
if native and native.solve_slider then
    -- Use fast Go implementation
    answer = native.solve_slider(content_json)
else
    -- Fall back to pure Lua implementation
    answer = solve_slider_lua(content_json)
end
```

Tier 1 native functions:
- `native.solve_slider(content_json)` → encoded answer string (current edge-matching algorithm)

Additional native functions added as needed — each is a Go function registered in the VM.

## File Structure

```
internal/captcha/
  lua.go                    -- LuaSolver: VM lifecycle, solve() entry point
  luamod/
    mod_http.go             -- http.* module
    mod_img.go              -- img.* module
    mod_crypto.go           -- crypto.* module
    mod_fp.go               -- fp.* module
    mod_json.go             -- json.* module
    mod_re.go               -- re.* module
    mod_url.go              -- url.* module
    mod_time.go             -- time.* module
    mod_log.go              -- log.* module
    mod_native.go           -- native.* module (Go-optimized functions)
    mod_config.go           -- config global (from vk-config.json)
    register.go             -- RegisterAll(L, opts) wires all modules
  lua_test.go               -- Integration tests with embedded test scripts

internal/scripts/bundled/
  solver.lua                -- Default bundled solver script
  vk-config.json            -- Config (unchanged)
  stealth.js                -- Chrome stealth (unchanged)
  manifest.json             -- Updated with solver.lua entry

hot-scripts/
  solver.lua                -- Remote-served copy
  vk-config.json
  stealth.js
  manifest.json
```

## solver.lua Contract

The script must define a global function `solve` that receives a table and returns a string:

```lua
function solve(challenge)
    -- challenge.redirect_uri  (string) -- captcha page URL
    -- challenge.captcha_sid   (string) -- VK captcha session ID
    -- challenge.captcha_ts    (number) -- VK captcha timestamp
    -- challenge.captcha_img   (string) -- fallback image URL (classic captcha)

    -- ... solving logic ...

    return success_token  -- string, or error("reason")
end
```

Go calls `solve(challenge)` and expects either:
- A string return value (success_token) → solver succeeded
- A Lua error → solver failed, ChainSolver tries next

## Error Handling

- **Lua runtime error** (syntax, nil access, etc.) → logged, solver returns error
- **Timeout** (30s) → VM cancelled via context, solver returns error
- **solver.lua not found** → LuaSolver returns error immediately, no VM created
- **API call failure** (HTTP error, image decode error) → Lua receives nil + error string, script decides whether to retry or error out
- **Script panic recovery** → Go defers recover(), logs stack trace

## Security

- Each solve creates a **fresh VM** — no state leakage between solves
- Lua has **no filesystem access** — only API modules provided by Go
- Lua has **no os.execute** — can't spawn processes
- HTTP requests are scoped to the solve context timeout
- Scripts are verified via SHA-256 hash in manifest (existing bundle integrity check)
- Remote scripts additionally require ED25519 signature

## Testing Strategy

1. **Unit tests per module** — each `mod_*.go` tested independently with small Lua snippets
2. **Integration test** — `lua_test.go` loads a test `solver.lua` that exercises all APIs against a mock HTTP server
3. **E2E test** — existing `test-e2e.sh` runs with LuaSolver as primary (replace DirectSolver)
4. **Regression** — keep DirectSolver code as reference; test that `solver.lua` produces identical results for same inputs

## Migration Path

1. Implement LuaSolver + Tier 1 API modules
2. Port DirectSolver logic to `solver.lua` (line-by-line translation)
3. Test: LuaSolver produces same results as DirectSolver
4. Swap LuaSolver into ChainSolver chains (all platforms)
5. Run E2E tests + Android deploy
6. Keep DirectSolver as `native.solve_slider` optimization

## Dependencies

- `github.com/yuin/gopher-lua` — Lua 5.1 VM, pure Go, no CGO, ~2MB binary size increase
- No other new dependencies — image processing uses stdlib `image/*`, crypto uses stdlib `crypto/*`

## Future Extensions (Tier 2+)

Not in scope for initial implementation, but designed to be addable:

- **Audio processing** (`mod_audio.go`) — for audio captcha fallback
- **ML inference** (`mod_ml.go`) — ONNX model loading for image classification
- **WebSocket** (`mod_ws.go`) — if VK moves captcha to WS protocol
- **MessagePack/Protobuf** — for binary captcha protocols
- **Canvas/WebGL fingerprint generation** — for advanced anti-bot evasion
- **TLS fingerprint control** — JA3/JA4 profile selection per request
