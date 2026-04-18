package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/call-vpn/call-vpn/internal/captcha/luamod"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/scripts"
	"github.com/call-vpn/call-vpn/internal/turn"
	lua "github.com/yuin/gopher-lua"
)

const authScriptFile = "auth.lua"

// runLuaAuth executes auth.lua in a fresh Lua VM and returns JoinInfo.
//
// The script receives:
//   - config   — parsed vk-config.json
//   - args     — {join_link, name, token}
//   - captcha  — {solve=function(challenge) → result}
//   - http, json, crypto, log, time, url, re — standard luamod modules
func runLuaAuth(ctx context.Context, mgr *scripts.Manager, solver provider.CaptchaSolver, joinLink, name, token string) (*provider.JoinInfo, error) {
	code, ok := mgr.File(authScriptFile)
	if !ok {
		return nil, fmt.Errorf("auth script not found: %s", authScriptFile)
	}

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()

	openSafeLibs(L)

	// Register standard Go-backed modules.
	opts := luamod.Options{
		Ctx:    ctx,
		Logger: slog.Default().With("component", "lua-auth"),
		HTTPClient: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}
	if c := hotVKConfig(); c != nil {
		configBytes, _ := json.Marshal(c)
		var configMap map[string]any
		if err := json.Unmarshal(configBytes, &configMap); err == nil {
			opts.Config = configMap
		}
	}
	luamod.RegisterAll(L, opts)

	// Register captcha.solve(challenge_table) callback.
	registerCaptchaModule(L, ctx, solver)

	// Set args global.
	argsTbl := L.NewTable()
	argsTbl.RawSetString("join_link", lua.LString(joinLink))
	argsTbl.RawSetString("name", lua.LString(name))
	argsTbl.RawSetString("token", lua.LString(token))
	L.SetGlobal("args", argsTbl)

	// Wire context for timeout/cancellation.
	L.SetContext(ctx)

	// Load and execute the script.
	if err := L.DoString(string(code)); err != nil {
		return nil, fmt.Errorf("auth script load: %w", err)
	}

	// Call fetch_join_info(args).
	fn := L.GetGlobal("fetch_join_info")
	if fn == lua.LNil {
		return nil, fmt.Errorf("auth script does not define fetch_join_info()")
	}

	if err := L.CallByParam(lua.P{
		Fn:      fn,
		NRet:    1,
		Protect: true,
	}, argsTbl); err != nil {
		return nil, classifyLuaError(err)
	}

	ret := L.Get(-1)
	L.Pop(1)

	tbl, ok2 := ret.(*lua.LTable)
	if !ok2 {
		return nil, fmt.Errorf("auth script returned %T, expected table", ret)
	}

	return parseLuaJoinInfo(tbl)
}

// registerCaptchaModule registers captcha.solve(challenge) → result callback.
func registerCaptchaModule(L *lua.LState, ctx context.Context, solver provider.CaptchaSolver) {
	mod := L.NewTable()

	solveFn := L.NewFunction(func(L *lua.LState) int {
		if solver == nil {
			L.RaiseError("no captcha solver configured")
			return 0
		}

		challengeTbl := L.CheckTable(1)

		ch := &provider.CaptchaChallenge{}
		if v, ok := challengeTbl.RawGetString("redirect_uri").(lua.LString); ok {
			ch.RedirectURI = string(v)
		}
		if v, ok := challengeTbl.RawGetString("captcha_sid").(lua.LString); ok {
			ch.CaptchaSID = string(v)
		}
		if v, ok := challengeTbl.RawGetString("captcha_ts").(lua.LNumber); ok {
			ch.CaptchaTS = float64(v)
		}
		if v, ok := challengeTbl.RawGetString("captcha_img").(lua.LString); ok {
			ch.CaptchaImg = string(v)
		}

		result, err := solver.SolveCaptcha(ctx, ch)
		if err != nil {
			L.RaiseError("captcha solve: %v", err)
			return 0
		}

		res := L.NewTable()
		res.RawSetString("success_token", lua.LString(result.SuccessToken))
		res.RawSetString("captcha_key", lua.LString(result.CaptchaKey))
		if result.RetryParams != nil {
			rp := L.NewTable()
			for k, v := range result.RetryParams {
				rp.RawSetString(k, lua.LString(v))
			}
			res.RawSetString("retry_params", rp)
		}

		L.Push(res)
		return 1
	})

	L.SetField(mod, "solve", solveFn)
	L.SetGlobal("captcha", mod)
}

// parseLuaJoinInfo converts a Lua table from auth.lua into provider.JoinInfo.
func parseLuaJoinInfo(tbl *lua.LTable) (*provider.JoinInfo, error) {
	username := luaString(tbl, "username")
	password := luaString(tbl, "password")
	wsEndpoint := luaString(tbl, "ws_endpoint")
	convID := luaString(tbl, "conv_id")
	deviceIdx := luaInt(tbl, "device_idx")

	// Parse TURN URLs.
	urlsTbl, ok := tbl.RawGetString("urls").(*lua.LTable)
	if !ok || urlsTbl.Len() == 0 {
		return nil, fmt.Errorf("auth script returned no TURN URLs")
	}

	var urls []string
	urlsTbl.ForEach(func(_, v lua.LValue) {
		if s, ok := v.(lua.LString); ok {
			urls = append(urls, string(s))
		}
	})

	if len(urls) == 0 {
		return nil, fmt.Errorf("auth script returned empty TURN URLs")
	}

	host, port := turn.ParseTURNURL(urls[0])
	var servers []provider.TURNServer
	for _, u := range urls {
		h, p := turn.ParseTURNURL(u)
		servers = append(servers, provider.TURNServer{Host: h, Port: p})
	}

	return &provider.JoinInfo{
		Credentials: provider.Credentials{
			Username: username,
			Password: password,
			Host:     host,
			Port:     port,
			Servers:  servers,
		},
		WSEndpoint: wsEndpoint,
		ConvID:     convID,
		DeviceIdx:  deviceIdx,
	}, nil
}

// classifyLuaError maps Lua error strings to Go error types.
func classifyLuaError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "rate_limit:") {
		// Extract code and message: "rate_limit:14:Captcha needed"
		parts := strings.SplitN(msg, "rate_limit:", 2)
		if len(parts) == 2 {
			subParts := strings.SplitN(parts[1], ":", 2)
			code := 0
			fmt.Sscanf(subParts[0], "%d", &code)
			errMsg := ""
			if len(subParts) == 2 {
				errMsg = subParts[1]
			}
			return &provider.RateLimitError{Code: code, Message: errMsg}
		}
	}
	return err
}

// openSafeLibs opens restricted Lua standard libraries (same as captcha/lua.go).
func openSafeLibs(L *lua.LState) {
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.LoadLibName, lua.OpenPackage},
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	L.SetGlobal("dofile", lua.LNil)
	L.SetGlobal("loadfile", lua.LNil)
}

// Lua table accessors.

func luaString(tbl *lua.LTable, key string) string {
	if v, ok := tbl.RawGetString(key).(lua.LString); ok {
		return string(v)
	}
	return ""
}

func luaInt(tbl *lua.LTable, key string) int {
	if v, ok := tbl.RawGetString(key).(lua.LNumber); ok {
		return int(v)
	}
	return 0
}
