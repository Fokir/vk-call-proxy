package luamod

import (
	"context"
	"log/slog"
	"net/http"

	lua "github.com/yuin/gopher-lua"
)

// Options configures the Lua modules registered in the VM.
type Options struct {
	Ctx        context.Context
	HTTPClient *http.Client
	Logger     *slog.Logger
	Config     map[string]interface{}
	// SolveSlider, if set, is exposed as native.solve_slider(content_json) → base64_answer.
	SolveSlider func(contentJSON []byte) (string, error)
}

// RegisterAll registers all Go-backed Lua modules into L.
func RegisterAll(L *lua.LState, opts Options) {
	RegisterJSON(L)
	RegisterCrypto(L)
	RegisterHTTP(L, opts)
	RegisterRe(L)
	RegisterURL(L)
	RegisterTime(L, opts.Ctx)
	RegisterLog(L, opts.Logger)
	RegisterFP(L)
	RegisterImg(L)
	RegisterConfig(L, opts)
	RegisterNative(L, opts)
}
