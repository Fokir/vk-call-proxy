package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/call-vpn/call-vpn/internal/captcha/luamod"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/scripts"
	lua "github.com/yuin/gopher-lua"
)

// LuaSolver runs a Lua script to solve captcha challenges.
// Each SolveCaptcha call creates a fresh VM.
type LuaSolver struct {
	mgr *scripts.Manager

	mu       sync.Mutex
	override []byte // script override for testing
}

// NewLuaSolver creates a LuaSolver that loads solver.lua from mgr.
func NewLuaSolver(mgr *scripts.Manager) *LuaSolver {
	return &LuaSolver{mgr: mgr}
}

// SetScript overrides the solver script (for testing).
func (s *LuaSolver) SetScript(code []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.override = code
}

// SolveCaptcha implements provider.CaptchaSolver.
func (s *LuaSolver) SolveCaptcha(ctx context.Context, ch *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	code, err := s.loadScript()
	if err != nil {
		return nil, err
	}

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()

	// Open only safe standard libraries.
	openSafeLibs(L)

	// Build module options.
	opts := luamod.Options{
		Ctx: ctx,
		SolveSlider: func(contentJSON []byte) (string, error) {
			puzzle, err := parseSliderContent(contentJSON)
			if err != nil {
				return "", err
			}
			answer, err := solveSlider(puzzle)
			if err != nil {
				return "", err
			}
			return encodeSliderAnswer(answer), nil
		},
	}
	if c := hotVKConfig(); c != nil {
		configBytes, _ := json.Marshal(c)
		var configMap map[string]interface{}
		if err := json.Unmarshal(configBytes, &configMap); err == nil {
			opts.Config = configMap
		}
	}

	// Register Go-backed modules.
	luamod.RegisterAll(L, opts)

	// Wire context for timeout/cancellation.
	L.SetContext(ctx)

	// Load and execute the script.
	if err := L.DoString(string(code)); err != nil {
		return nil, fmt.Errorf("lua script load: %w", err)
	}

	// Build challenge table.
	tbl := buildChallengeTable(L, ch)

	// Call global solve(challenge).
	solveFn := L.GetGlobal("solve")
	if solveFn == lua.LNil {
		return nil, errors.New("lua script does not define a global 'solve' function")
	}

	if err := L.CallByParam(lua.P{
		Fn:      solveFn,
		NRet:    1,
		Protect: true,
	}, tbl); err != nil {
		return nil, fmt.Errorf("lua solve(): %w", err)
	}

	ret := L.Get(-1)
	L.Pop(1)

	// Parse result: either a string (success_token) or a table.
	switch v := ret.(type) {
	case *lua.LNilType:
		return nil, errors.New("lua solve() returned nil")
	case lua.LString:
		return &provider.CaptchaResult{SuccessToken: string(v)}, nil
	case *lua.LTable:
		token := v.RawGetString("success_token")
		key := v.RawGetString("captcha_key")
		res := &provider.CaptchaResult{}
		if s, ok := token.(lua.LString); ok {
			res.SuccessToken = string(s)
		}
		if s, ok := key.(lua.LString); ok {
			res.CaptchaKey = string(s)
		}
		if res.SuccessToken == "" && res.CaptchaKey == "" {
			return nil, errors.New("lua solve() returned table with no success_token or captcha_key")
		}
		return res, nil
	default:
		return nil, fmt.Errorf("lua solve() returned unexpected type %T", ret)
	}
}

// loadScript returns the Lua script code to execute.
func (s *LuaSolver) loadScript() ([]byte, error) {
	s.mu.Lock()
	override := s.override
	s.mu.Unlock()

	if override != nil {
		return override, nil
	}

	if s.mgr != nil {
		if data, ok := s.mgr.File("solver.lua"); ok {
			return data, nil
		}
	}

	return nil, errors.New("no solver.lua script available")
}

// openSafeLibs opens a restricted set of Lua standard libraries.
// Intentionally excludes io, os, debug, loadfile, dofile.
func openSafeLibs(L *lua.LState) {
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.LoadLibName, lua.OpenPackage}, // for require/module basics
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}

	// Remove dangerous globals that base lib exposes.
	L.SetGlobal("dofile", lua.LNil)
	L.SetGlobal("loadfile", lua.LNil)
}

// buildChallengeTable converts a CaptchaChallenge to a Lua table.
func buildChallengeTable(L *lua.LState, ch *provider.CaptchaChallenge) *lua.LTable {
	tbl := L.NewTable()
	tbl.RawSetString("redirect_uri", lua.LString(ch.RedirectURI))
	tbl.RawSetString("captcha_sid", lua.LString(ch.CaptchaSID))
	tbl.RawSetString("captcha_ts", lua.LNumber(ch.CaptchaTS))
	tbl.RawSetString("captcha_img", lua.LString(ch.CaptchaImg))
	return tbl
}
