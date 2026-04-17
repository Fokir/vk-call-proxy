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
}

// RegisterAll registers all Go-backed Lua modules into L.
// Modules will be added in later tasks.
func RegisterAll(L *lua.LState, opts Options) {
	// Empty for now — modules added in later tasks.
}
