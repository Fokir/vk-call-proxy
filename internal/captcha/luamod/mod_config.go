package luamod

import (
	lua "github.com/yuin/gopher-lua"
)

// RegisterConfig registers the `config` global table into L.
// If opts.Config is nil, an empty table is registered so scripts can safely
// access config fields (e.g. config.captcha) without crashing.
func RegisterConfig(L *lua.LState, opts Options) {
	if opts.Config != nil {
		L.SetGlobal("config", GoToLua(L, opts.Config))
	} else {
		L.SetGlobal("config", L.NewTable())
	}
}
