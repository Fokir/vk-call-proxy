# LuaSolver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace hardcoded DirectSolver with a Lua VM-based captcha solver where the solving algorithm is hot-updatable via the existing scripts bundle system.

**Architecture:** `LuaSolver` creates a fresh `gopher-lua` VM per solve, loads `solver.lua` from `scripts.Manager`, registers Go-backed API modules (http, img, crypto, fp, json, re, url, time, log, native, config), and calls `solve(challenge)`. Falls back to next solver in ChainSolver if script is missing or fails.

**Tech Stack:** Go 1.25.7, `github.com/yuin/gopher-lua` (pure Go Lua 5.1 VM), existing `internal/scripts` hot-bundle system.

**Spec:** `docs/superpowers/specs/2026-04-17-lua-captcha-solver-design.md`
**API Reference:** `internal/scripts/LUA_API.md`

---

## File Structure

```
internal/captcha/
  lua.go                 -- LuaSolver struct, SolveCaptcha(), VM lifecycle
  luamod/
    register.go          -- RegisterAll() wires all modules into VM
    mod_http.go          -- http.get, http.post, http.post_form, http.post_json
    mod_img.go           -- img.load, img.crop, img.pixel, img.edge_detect, etc.
    mod_crypto.go        -- crypto.sha256, crypto.base64_encode, crypto.pow_solve, etc.
    mod_fp.go            -- fp.mouse_path, fp.device_info, fp.browser_fp, etc.
    mod_json.go          -- json.encode, json.decode
    mod_re.go            -- re.match, re.find_all, re.replace
    mod_url.go           -- url.encode, url.decode, url.parse
    mod_time.go          -- time.sleep, time.now
    mod_log.go           -- log.info, log.debug, log.warn
    mod_native.go        -- native.solve_slider (Go-optimized)
    mod_config.go        -- config global table from vk-config.json
  lua_test.go            -- LuaSolver integration tests

internal/scripts/bundled/
  solver.lua             -- Default bundled solver (port of DirectSolver)
  manifest.json          -- Updated with solver.lua entry
```

---

### Task 1: Add gopher-lua dependency

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Add dependency**

```bash
go get github.com/yuin/gopher-lua@latest
```

- [ ] **Step 2: Tidy**

```bash
go mod tidy
```

- [ ] **Step 3: Verify**

```bash
go build ./...
```

Expected: builds with no errors, `gopher-lua` in go.mod.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: add gopher-lua dependency"
```

---

### Task 2: Lua module registration scaffold + LuaSolver skeleton

**Files:**
- Create: `internal/captcha/luamod/register.go`
- Create: `internal/captcha/lua.go`
- Test: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write failing test for LuaSolver**

Create `internal/captcha/lua_test.go`:

```go
package captcha

import (
	"context"
	"testing"

	"github.com/call-vpn/call-vpn/internal/provider"
)

func TestLuaSolver_SimpleReturn(t *testing.T) {
	solver := NewLuaSolver(nil) // nil scripts manager = use embedded script
	solver.SetScript([]byte(`
		function solve(challenge)
			return "test-token-123"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		RedirectURI: "https://id.vk.com/captcha?session_token=abc",
		CaptchaSID:  "12345",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SuccessToken != "test-token-123" {
		t.Fatalf("expected test-token-123, got %s", result.SuccessToken)
	}
}

func TestLuaSolver_ScriptError(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			error("captcha failed")
		end
	`))

	_, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		CaptchaSID: "12345",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestLuaSolver_NoScript(t *testing.T) {
	solver := NewLuaSolver(nil)
	// No SetScript, no scripts manager → should error

	_, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		CaptchaSID: "12345",
	})
	if err == nil {
		t.Fatal("expected error when no script available")
	}
}

func TestLuaSolver_Timeout(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			while true do end
		end
	`))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := solver.SolveCaptcha(ctx, &provider.CaptchaChallenge{
		CaptchaSID: "12345",
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
```

- [ ] **Step 2: Run tests — verify they fail**

```bash
go test -run "TestLuaSolver" -v ./internal/captcha/
```

Expected: compilation errors (NewLuaSolver not defined).

- [ ] **Step 3: Create register.go scaffold**

Create `internal/captcha/luamod/register.go`:

```go
package luamod

import (
	"context"
	"log/slog"
	"net/http"

	lua "github.com/yuin/gopher-lua"
)

// Options configures the Lua API modules.
type Options struct {
	Ctx        context.Context
	HTTPClient *http.Client
	Logger     *slog.Logger
	Config     map[string]interface{} // from vk-config.json
}

// RegisterAll registers all Go-backed API modules into the Lua VM.
func RegisterAll(L *lua.LState, opts Options) {
	// Modules will be registered here as they are implemented.
	// Each mod_*.go file adds its own Register* function.
}
```

- [ ] **Step 4: Create lua.go with LuaSolver**

Create `internal/captcha/lua.go`:

```go
package captcha

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/call-vpn/call-vpn/internal/captcha/luamod"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/scripts"

	lua "github.com/yuin/gopher-lua"
)

const luaSolverFile = "solver.lua"

// LuaSolver runs a Lua script to solve captchas. The script is loaded from
// the hot-bundle (scripts.Manager) or from an explicit SetScript call.
// Each SolveCaptcha invocation creates a fresh VM for isolation.
type LuaSolver struct {
	mgr    *scripts.Manager
	logger *slog.Logger

	mu       sync.Mutex
	override []byte // explicit script (testing / fallback)
}

// NewLuaSolver creates a solver that loads solver.lua from the scripts manager.
// If mgr is nil, only explicitly set scripts (via SetScript) are used.
func NewLuaSolver(mgr *scripts.Manager) *LuaSolver {
	return &LuaSolver{
		mgr:    mgr,
		logger: slog.Default().With("component", "lua-solver"),
	}
}

// SetScript sets an explicit Lua script to use instead of loading from the
// scripts manager. Primarily for testing.
func (s *LuaSolver) SetScript(code []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.override = code
}

func (s *LuaSolver) loadScript() ([]byte, error) {
	s.mu.Lock()
	override := s.override
	s.mu.Unlock()

	if override != nil {
		return override, nil
	}
	if s.mgr != nil {
		if data, ok := s.mgr.File(luaSolverFile); ok {
			return data, nil
		}
	}
	return nil, fmt.Errorf("solver.lua not found (no script set, no bundle)")
}

func (s *LuaSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	code, err := s.loadScript()
	if err != nil {
		return nil, err
	}

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()

	// Open safe subset of standard libs.
	for _, pair := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.LoadLibName, lua.OpenPackage},
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(pair.fn))
		L.Push(lua.LString(pair.name))
		L.Call(1, 0)
	}

	// Remove dangerous globals.
	L.SetGlobal("dofile", lua.LNil)
	L.SetGlobal("loadfile", lua.LNil)

	// Register API modules.
	luamod.RegisterAll(L, luamod.Options{
		Ctx:        ctx,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Logger:     s.logger,
	})

	// Set context cancel hook (for timeout).
	L.SetContext(ctx)

	// Load and execute the script.
	if err := L.DoString(string(code)); err != nil {
		return nil, fmt.Errorf("lua load: %w", err)
	}

	// Build challenge table.
	challengeT := L.NewTable()
	challengeT.RawSetString("redirect_uri", lua.LString(ch.RedirectURI))
	challengeT.RawSetString("captcha_sid", lua.LString(ch.CaptchaSID))
	challengeT.RawSetString("captcha_ts", lua.LNumber(ch.CaptchaTS))
	challengeT.RawSetString("captcha_img", lua.LString(ch.CaptchaImg))

	// Call solve(challenge).
	solveFn := L.GetGlobal("solve")
	if solveFn == lua.LNil {
		return nil, fmt.Errorf("solver.lua: no global 'solve' function")
	}

	if err := L.CallByParam(lua.P{
		Fn:      solveFn,
		NRet:    1,
		Protect: true,
	}, challengeT); err != nil {
		return nil, fmt.Errorf("lua solve: %w", err)
	}

	token := L.Get(-1)
	L.Pop(1)

	tokenStr, ok := token.(lua.LString)
	if !ok || tokenStr == "" {
		return nil, fmt.Errorf("solver.lua returned non-string or empty: %v", token)
	}

	return &provider.CaptchaResult{SuccessToken: string(tokenStr)}, nil
}
```

- [ ] **Step 5: Add missing import to test file**

Add `"time"` import to `lua_test.go`.

- [ ] **Step 6: Run tests**

```bash
go test -run "TestLuaSolver" -v -timeout 30s ./internal/captcha/
```

Expected: all 4 tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/captcha/lua.go internal/captcha/luamod/register.go internal/captcha/lua_test.go
git commit -m "feat(captcha): add LuaSolver skeleton with gopher-lua VM"
```

---

### Task 3: mod_json — JSON encode/decode

**Files:**
- Create: `internal/captcha/luamod/mod_json.go`
- Modify: `internal/captcha/luamod/register.go`
- Modify: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write failing test**

Append to `lua_test.go`:

```go
func TestLuaMod_JSON(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			local t = json.decode('{"status":"OK","token":"abc"}')
			if t.status ~= "OK" then error("decode failed") end
			local s = json.encode({result = t.token, num = 42})
			-- s should contain "abc" and "42"
			if not string.find(s, "abc") then error("encode missing abc") end
			return t.token
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "abc" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}
```

- [ ] **Step 2: Run test — verify it fails**

```bash
go test -run "TestLuaMod_JSON" -v ./internal/captcha/
```

Expected: FAIL — `json` not registered in Lua.

- [ ] **Step 3: Implement mod_json.go**

Create `internal/captcha/luamod/mod_json.go`:

```go
package luamod

import (
	"encoding/json"

	lua "github.com/yuin/gopher-lua"
)

func RegisterJSON(L *lua.LState) {
	mod := L.NewTable()
	L.SetField(mod, "decode", L.NewFunction(jsonDecode))
	L.SetField(mod, "encode", L.NewFunction(jsonEncode))
	L.SetGlobal("json", mod)
}

func jsonDecode(L *lua.LState) int {
	str := L.CheckString(1)
	var v interface{}
	if err := json.Unmarshal([]byte(str), &v); err != nil {
		L.ArgError(1, "invalid JSON: "+err.Error())
		return 0
	}
	L.Push(goToLua(L, v))
	return 1
}

func jsonEncode(L *lua.LState) int {
	val := L.CheckAny(1)
	goVal := luaToGo(val)
	data, err := json.Marshal(goVal)
	if err != nil {
		L.ArgError(1, "json encode: "+err.Error())
		return 0
	}
	L.Push(lua.LString(string(data)))
	return 1
}

// goToLua converts a Go value (from json.Unmarshal) to Lua value.
func goToLua(L *lua.LState, v interface{}) lua.LValue {
	switch val := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(val)
	case float64:
		return lua.LNumber(val)
	case string:
		return lua.LString(val)
	case []interface{}:
		t := L.NewTable()
		for i, item := range val {
			t.RawSetInt(i+1, goToLua(L, item))
		}
		return t
	case map[string]interface{}:
		t := L.NewTable()
		for k, item := range val {
			t.RawSetString(k, goToLua(L, item))
		}
		return t
	default:
		return lua.LString(json.Marshal(val))
	}
}

// luaToGo converts a Lua value to a Go value (for json.Marshal).
func luaToGo(val lua.LValue) interface{} {
	switch v := val.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(v)
	case lua.LNumber:
		return float64(v)
	case lua.LString:
		return string(v)
	case *lua.LTable:
		// Detect array vs object: check if sequential integer keys 1..N.
		maxN := v.MaxN()
		if maxN > 0 {
			arr := make([]interface{}, 0, maxN)
			for i := 1; i <= maxN; i++ {
				arr = append(arr, luaToGo(v.RawGetInt(i)))
			}
			return arr
		}
		obj := make(map[string]interface{})
		v.ForEach(func(key, value lua.LValue) {
			if ks, ok := key.(lua.LString); ok {
				obj[string(ks)] = luaToGo(value)
			}
		})
		return obj
	default:
		return val.String()
	}
}
```

- [ ] **Step 4: Wire into RegisterAll**

Modify `internal/captcha/luamod/register.go`, add to `RegisterAll`:

```go
func RegisterAll(L *lua.LState, opts Options) {
	RegisterJSON(L)
}
```

- [ ] **Step 5: Run test**

```bash
go test -run "TestLuaMod_JSON" -v ./internal/captcha/
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/captcha/luamod/mod_json.go internal/captcha/luamod/register.go internal/captcha/lua_test.go
git commit -m "feat(captcha): add Lua json module (encode/decode)"
```

---

### Task 4: mod_http — HTTP client

**Files:**
- Create: `internal/captcha/luamod/mod_http.go`
- Modify: `internal/captcha/luamod/register.go`
- Modify: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write failing test**

Append to `lua_test.go`:

```go
func TestLuaMod_HTTP(t *testing.T) {
	// Start test HTTP server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/get":
			w.Header().Set("X-Test", "hello")
			fmt.Fprintf(w, `{"method":"GET","ua":"%s"}`, r.Header.Get("User-Agent"))
		case "/post":
			body, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, `{"body":"%s"}`, string(body))
		case "/form":
			r.ParseForm()
			fmt.Fprintf(w, `{"field1":"%s","field2":"%s"}`, r.FormValue("field1"), r.FormValue("field2"))
		}
	}))
	defer ts.Close()

	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(fmt.Sprintf(`
		function solve(challenge)
			-- Test GET
			local resp = http.get("%s/get", {["User-Agent"] = "TestBot/1.0"})
			if resp.status ~= 200 then error("GET status " .. resp.status) end
			local data = json.decode(resp.body)
			if data.ua ~= "TestBot/1.0" then error("UA mismatch") end

			-- Test POST form
			local resp2 = http.post_form("%s/form", {}, {field1 = "hello", field2 = "world"})
			local data2 = json.decode(resp2.body)
			if data2.field1 ~= "hello" then error("form field1 mismatch") end

			return "http-ok"
		end
	`, ts.URL, ts.URL)))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "http-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}
```

- [ ] **Step 2: Run test — verify failure**

- [ ] **Step 3: Implement mod_http.go**

Create `internal/captcha/luamod/mod_http.go` with:
- `http.get(url, headers_table)` → `{status, body, headers}`
- `http.post(url, headers_table, body_string)` → `{status, body, headers}`
- `http.post_form(url, headers_table, fields_table)` → `{status, body, headers}`
- `http.post_json(url, headers_table, data_table)` → `{status, body, headers}`
- Shared `net/http.Client` with cookie jar from `Options`
- Context from `Options.Ctx` for timeout

```go
package luamod

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

func RegisterHTTP(L *lua.LState, opts Options) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: opts.HTTPClient.Timeout,
	}
	ctx := opts.Ctx

	mod := L.NewTable()
	L.SetField(mod, "get", L.NewFunction(func(L *lua.LState) int {
		return httpDo(L, ctx, client, "GET")
	}))
	L.SetField(mod, "post", L.NewFunction(func(L *lua.LState) int {
		return httpDo(L, ctx, client, "POST")
	}))
	L.SetField(mod, "post_form", L.NewFunction(func(L *lua.LState) int {
		return httpDoForm(L, ctx, client)
	}))
	L.SetField(mod, "post_json", L.NewFunction(func(L *lua.LState) int {
		return httpDoJSON(L, ctx, client)
	}))
	L.SetGlobal("http", mod)
}

func httpDo(L *lua.LState, ctx context.Context, client *http.Client, method string) int {
	rawURL := L.CheckString(1)
	headers := L.OptTable(2, L.NewTable())

	var bodyReader io.Reader
	if method == "POST" {
		bodyStr := L.OptString(3, "")
		bodyReader = strings.NewReader(bodyStr)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		L.ArgError(1, err.Error())
		return 0
	}

	headers.ForEach(func(k, v lua.LValue) {
		req.Header.Set(k.String(), v.String())
	})

	resp, err := client.Do(req)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return pushResponse(L, resp, body)
}

func httpDoForm(L *lua.LState, ctx context.Context, client *http.Client) int {
	rawURL := L.CheckString(1)
	headers := L.OptTable(2, L.NewTable())
	fields := L.CheckTable(3)

	form := url.Values{}
	fields.ForEach(func(k, v lua.LValue) {
		form.Set(k.String(), v.String())
	})

	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		L.ArgError(1, err.Error())
		return 0
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	headers.ForEach(func(k, v lua.LValue) {
		req.Header.Set(k.String(), v.String())
	})

	resp, err := client.Do(req)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return pushResponse(L, resp, body)
}

func httpDoJSON(L *lua.LState, ctx context.Context, client *http.Client) int {
	rawURL := L.CheckString(1)
	headers := L.OptTable(2, L.NewTable())
	data := L.CheckTable(3)

	jsonBytes, err := json.Marshal(luaToGo(data))
	if err != nil {
		L.ArgError(3, "json encode: "+err.Error())
		return 0
	}

	req, err := http.NewRequestWithContext(ctx, "POST", rawURL, strings.NewReader(string(jsonBytes)))
	if err != nil {
		L.ArgError(1, err.Error())
		return 0
	}
	req.Header.Set("Content-Type", "application/json")

	headers.ForEach(func(k, v lua.LValue) {
		req.Header.Set(k.String(), v.String())
	})

	resp, err := client.Do(req)
	if err != nil {
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))
		return 2
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return pushResponse(L, resp, body)
}

func pushResponse(L *lua.LState, resp *http.Response, body []byte) int {
	t := L.NewTable()
	t.RawSetString("status", lua.LNumber(resp.StatusCode))
	t.RawSetString("body", lua.LString(string(body)))

	hdrs := L.NewTable()
	for k, vals := range resp.Header {
		if len(vals) > 0 {
			hdrs.RawSetString(k, lua.LString(vals[0]))
		}
	}
	t.RawSetString("headers", hdrs)

	L.Push(t)
	return 1
}
```

- [ ] **Step 4: Wire into RegisterAll**

```go
func RegisterAll(L *lua.LState, opts Options) {
	RegisterJSON(L)
	RegisterHTTP(L, opts)
}
```

- [ ] **Step 5: Add imports to test file**

Add `"fmt"`, `"io"`, `"net/http"`, `"net/http/httptest"` to test imports.

- [ ] **Step 6: Run test**

```bash
go test -run "TestLuaMod_HTTP" -v ./internal/captcha/
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/captcha/luamod/mod_http.go internal/captcha/luamod/register.go internal/captcha/lua_test.go
git commit -m "feat(captcha): add Lua http module (get/post/form/json)"
```

---

### Task 5: mod_crypto — Hashing, encoding, PoW

**Files:**
- Create: `internal/captcha/luamod/mod_crypto.go`
- Modify: `internal/captcha/luamod/register.go`
- Modify: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestLuaMod_Crypto(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			-- SHA-256
			local h = crypto.sha256("hello")
			if h ~= "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" then
				error("sha256 mismatch: " .. h)
			end

			-- Base64
			local enc = crypto.base64_encode("hello world")
			local dec = crypto.base64_decode(enc)
			if dec ~= "hello world" then error("base64 roundtrip failed") end

			-- PoW (difficulty 1 = easy)
			local nonce = crypto.pow_solve("test", 1)
			if nonce == "" then error("pow failed") end

			-- Random bytes
			local r = crypto.random_bytes(16)
			if #r ~= 16 then error("random_bytes length") end

			return "crypto-ok"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "crypto-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}
```

- [ ] **Step 2: Run test — verify failure**

- [ ] **Step 3: Implement mod_crypto.go**

Create `internal/captcha/luamod/mod_crypto.go` with functions:
- `crypto.sha256(data)` → hex
- `crypto.sha1(data)` → hex
- `crypto.md5(data)` → hex
- `crypto.hmac_sha256(key, data)` → hex
- `crypto.base64_encode(data)` → string
- `crypto.base64_decode(string)` → data
- `crypto.random_bytes(n)` → string (raw bytes)
- `crypto.pow_solve(prefix, difficulty)` → nonce string
- `crypto.xor(data1, data2)` → string
- `crypto.aes_encrypt(key, iv, plaintext)` → string
- `crypto.aes_decrypt(key, iv, ciphertext)` → string

All use Go stdlib `crypto/*`. `pow_solve` reuses logic from `internal/captcha/direct.go:computeProofOfWork`.

- [ ] **Step 4: Wire into RegisterAll, run test**

- [ ] **Step 5: Commit**

```bash
git add internal/captcha/luamod/mod_crypto.go internal/captcha/luamod/register.go internal/captcha/lua_test.go
git commit -m "feat(captcha): add Lua crypto module (sha256/base64/pow/aes)"
```

---

### Task 6: mod_re, mod_url, mod_time, mod_log — Utility modules

**Files:**
- Create: `internal/captcha/luamod/mod_re.go`
- Create: `internal/captcha/luamod/mod_url.go`
- Create: `internal/captcha/luamod/mod_time.go`
- Create: `internal/captcha/luamod/mod_log.go`
- Modify: `internal/captcha/luamod/register.go`
- Modify: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestLuaMod_Utils(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			-- Regex
			local m = re.match("token=([^&]+)", "foo=bar&token=abc123&x=1")
			if m[1] ~= "abc123" then error("re.match: " .. tostring(m[1])) end

			local all = re.find_all("\\d+", "a1b23c456")
			if #all ~= 3 then error("re.find_all count: " .. #all) end

			-- URL
			local enc = url.encode("hello world&foo")
			if not string.find(enc, "hello") then error("url.encode") end
			local dec = url.decode(enc)
			if dec ~= "hello world&foo" then error("url.decode") end

			-- Time
			local now = time.now()
			if now < 1000000000000 then error("time.now too small") end

			-- Log (just verify no crash)
			log.info("test message", "key", "value")
			log.debug("debug msg")

			return "utils-ok"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "utils-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}
```

- [ ] **Step 2: Implement all four modules**

Each is small (~30-50 lines). `re.*` wraps Go `regexp`, `url.*` wraps `net/url`, `time.*` uses `time.Sleep` + `time.Now().UnixMilli()`, `log.*` uses `slog`.

- [ ] **Step 3: Wire into RegisterAll, run test**

- [ ] **Step 4: Commit**

```bash
git add internal/captcha/luamod/mod_re.go internal/captcha/luamod/mod_url.go \
        internal/captcha/luamod/mod_time.go internal/captcha/luamod/mod_log.go \
        internal/captcha/luamod/register.go internal/captcha/lua_test.go
git commit -m "feat(captcha): add Lua re/url/time/log modules"
```

---

### Task 7: mod_fp — Fingerprint generation

**Files:**
- Create: `internal/captcha/luamod/mod_fp.go`
- Modify: `internal/captcha/luamod/register.go`
- Modify: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestLuaMod_FP(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			local points = fp.mouse_path(100, 300, 400, 300, 20)
			if #points < 10 then error("mouse_path too short: " .. #points) end
			if points[1].x == nil then error("missing x") end

			local dev = fp.device_info(nil)
			if dev.screenWidth < 1000 then error("screen too small") end

			local hash = fp.browser_fp(nil)
			if #hash ~= 32 then error("browser_fp length: " .. #hash) end

			local rtt = fp.connection_rtt(5)
			if #rtt ~= 5 then error("rtt count") end

			return "fp-ok"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "fp-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}
```

- [ ] **Step 2: Implement mod_fp.go**

Port logic from `internal/captcha/direct.go` functions: `generateSliderCursor`, `generateDeviceInfo`, `generateBrowserFp`, `generateConnectionRtt`, `generateConnectionDownlink`, `randomAdFp`. Expose as Lua functions returning tables.

- [ ] **Step 3: Wire, test, commit**

```bash
git commit -m "feat(captcha): add Lua fp module (mouse_path/device_info/browser_fp)"
```

---

### Task 8: mod_img — Image processing (Tier 1)

**Files:**
- Create: `internal/captcha/luamod/mod_img.go`
- Modify: `internal/captcha/luamod/register.go`
- Modify: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestLuaMod_Img(t *testing.T) {
	// Create a small test PNG in memory
	testImg := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for x := 0; x < 10; x++ {
		for y := 0; y < 10; y++ {
			testImg.Set(x, y, color.RGBA{uint8(x * 25), uint8(y * 25), 128, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, testImg)
	imgBase64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(fmt.Sprintf(`
		function solve(challenge)
			local image = img.decode_base64("%s")
			if img.width(image) ~= 10 then error("width") end
			if img.height(image) ~= 10 then error("height") end

			local r, g, b, a = img.pixel(image, 0, 0)
			if a ~= 255 then error("alpha") end

			local cropped = img.crop(image, 2, 2, 5, 5)
			if img.width(cropped) ~= 5 then error("crop width") end

			local gray = img.grayscale(image)
			if img.width(gray) ~= 10 then error("gray width") end

			return "img-ok"
		end
	`, imgBase64)))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "img-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}
```

- [ ] **Step 2: Implement mod_img.go**

Images stored as `*lua.LUserData` wrapping Go `image.Image`. Tier 1 functions:
- `img.load(bytes)`, `img.decode_base64(str)` — PNG/JPEG decode
- `img.width(ud)`, `img.height(ud)`, `img.pixel(ud, x, y)`
- `img.crop(ud, x, y, w, h)`, `img.resize(ud, w, h)`
- `img.grayscale(ud)`, `img.edge_detect(ud)`
- `img.template_match(ud, ud)`, `img.diff(ud, ud)`
- `img.encode(ud, format)`

Use Go stdlib `image`, `image/png`, `image/jpeg`. Edge detection: simple Sobel operator. Template matching: normalized cross-correlation.

- [ ] **Step 3: Wire, test, commit**

```bash
git commit -m "feat(captcha): add Lua img module (load/crop/pixel/edge_detect)"
```

---

### Task 9: mod_native + mod_config — Go-optimized functions and config

**Files:**
- Create: `internal/captcha/luamod/mod_native.go`
- Create: `internal/captcha/luamod/mod_config.go`
- Modify: `internal/captcha/luamod/register.go`
- Modify: `internal/captcha/lua.go` (pass config to opts)
- Modify: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestLuaMod_NativeSlider(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			if native and native.solve_slider then
				-- Just verify it's callable (real test needs actual puzzle data)
				return "native-available"
			end
			return "native-missing"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "native-available" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}

func TestLuaMod_Config(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			if config == nil then error("config nil") end
			return "config-ok"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "config-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}
```

- [ ] **Step 2: Implement mod_native.go**

Register `native.solve_slider(content_json_string)` → calls `parseSliderContent` + `solveSlider` + `encodeSliderAnswer` from `internal/captcha/slider.go`. Expose those functions as package-level (may need to make them exported or move to shared package).

- [ ] **Step 3: Implement mod_config.go**

Reads `hotVKConfig()` (from `scripts_provider.go`) and converts to nested Lua table. Set as `config` global.

- [ ] **Step 4: Wire, test, commit**

```bash
git commit -m "feat(captcha): add Lua native and config modules"
```

---

### Task 10: Port DirectSolver to solver.lua

**Files:**
- Create: `internal/scripts/bundled/solver.lua`
- Modify: `internal/scripts/bundled/manifest.json`
- Modify: `internal/captcha/lua_test.go`

- [ ] **Step 1: Write solver.lua**

Port the full flow from `internal/captcha/direct.go:solveDirectAPI` to Lua. Use the example from `internal/scripts/LUA_API.md` as starting point, expanding to handle both checkbox and slider types. The slider solving uses `native.solve_slider` with Lua fallback.

- [ ] **Step 2: Update manifest**

Run `bash scripts/resign-bundled.sh` to update SHA256 and size for solver.lua.

- [ ] **Step 3: Write integration test**

```go
func TestLuaSolver_BundledScript(t *testing.T) {
	// Verify the bundled solver.lua loads and parses without errors.
	mgr := testScriptsManager(t) // helper that loads bundled FS
	solver := NewLuaSolver(mgr)

	// Can't test against real VK, but verify script loads and
	// fails gracefully with invalid redirect_uri.
	_, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		RedirectURI: "https://id.vk.com/captcha?session_token=invalid",
		CaptchaSID:  "test",
	})
	// Expected: HTTP error (invalid token), not Lua error
	if err == nil {
		t.Fatal("expected error with invalid session_token")
	}
	t.Logf("expected error: %v", err)
}
```

- [ ] **Step 4: Commit**

```bash
git add internal/scripts/bundled/solver.lua internal/scripts/bundled/manifest.json internal/captcha/lua_test.go
git commit -m "feat(captcha): add bundled solver.lua (port of DirectSolver)"
```

---

### Task 11: Wire LuaSolver into all binaries

**Files:**
- Modify: `cmd/server/main.go`
- Modify: `cmd/client/main.go`
- Modify: `cmd/captcha-service/main.go`
- Modify: `mobile/bind/tunnel.go`

- [ ] **Step 1: Replace DirectSolver with LuaSolver in each binary**

In each file, change `captcha.NewDirectSolver()` to `captcha.NewLuaSolver(scriptsMgr)`.

Example for `mobile/bind/tunnel.go`:

```go
// Before:
solvers := []provider.CaptchaSolver{captcha.NewDirectSolver()}

// After:
solvers := []provider.CaptchaSolver{captcha.NewLuaSolver(t.scripts)}
```

- [ ] **Step 2: Build all targets**

```bash
go build ./cmd/server
go build ./cmd/client
go build ./cmd/captcha-service
gomobile bind -target android -androidapi 24 -ldflags "-checklinkname=0" -o mobile/android/app/libs/bind.aar ./mobile/bind/
```

Expected: all build successfully.

- [ ] **Step 3: Commit**

```bash
git add cmd/server/main.go cmd/client/main.go cmd/captcha-service/main.go mobile/bind/tunnel.go
git commit -m "feat(captcha): wire LuaSolver as primary solver in all binaries"
```

---

### Task 12: E2E test — desktop

**Files:** none (test only)

- [ ] **Step 1: Run unit tests**

```bash
go test -timeout 600s ./internal/captcha/ ./internal/captcha/luamod/
```

Expected: all pass.

- [ ] **Step 2: Run E2E test**

```bash
bash test-e2e.sh --n=1 --vk-token
```

Expected: captcha solved by LuaSolver (check logs for `component=lua-solver`), tunnel connects.

- [ ] **Step 3: If E2E fails, debug and fix solver.lua**

Check logs for Lua errors. Fix solver.lua, re-run resign-bundled.sh, rebuild.

---

### Task 13: E2E test — Android

**Files:** none (test only)

- [ ] **Step 1: Build and deploy APK**

```bash
bash scripts/deploy-debug.sh
```

- [ ] **Step 2: Clear scripts cache on device**

```bash
adb shell "su -c 'rm -rf /data/data/com.callvpn.app/files/scripts/current'"
```

- [ ] **Step 3: Start server and Android app**

Start server with `--link`, launch app, press Connect. Verify:
- Logs show `scripts: loaded local bundle` with solver.lua
- Captcha solved automatically (no WebView fallback)
- Tunnel connects

- [ ] **Step 4: Commit final**

```bash
git commit -m "test(captcha): verify LuaSolver E2E on desktop and Android"
```

---

### Task 14: Update hot-scripts remote copy

**Files:**
- Copy: `hot-scripts/solver.lua` (from bundled)
- CI will auto-sign on push

- [ ] **Step 1: Sync solver.lua to hot-scripts**

```bash
cp internal/scripts/bundled/solver.lua hot-scripts/solver.lua
```

- [ ] **Step 2: Commit and push**

```bash
git add hot-scripts/solver.lua
git commit -m "chore(scripts): add solver.lua to hot-scripts"
git push
```

CI workflow `scripts-publish.yml` will sign the manifest automatically.
