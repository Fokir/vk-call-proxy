package luamod

import (
	lua "github.com/yuin/gopher-lua"
)

// RegisterNative registers the `native` global table into L.
// If opts.SolveSlider is set, native.solve_slider is registered.
func RegisterNative(L *lua.LState, opts Options) {
	tbl := L.NewTable()

	if opts.SolveSlider != nil {
		fn := opts.SolveSlider // capture
		L.SetField(tbl, "solve_slider", L.NewFunction(func(L *lua.LState) int {
			jsonStr := L.CheckString(1)
			result, err := fn([]byte(jsonStr))
			if err != nil {
				L.RaiseError("native.solve_slider: %v", err)
				return 0
			}
			L.Push(lua.LString(result))
			return 1
		}))
	}

	L.SetGlobal("native", tbl)
}
