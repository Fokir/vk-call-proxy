package luamod

import (
	"log/slog"

	lua "github.com/yuin/gopher-lua"
)

// RegisterLog registers the `log` global table into L.
func RegisterLog(L *lua.LState, logger *slog.Logger) {
	tbl := L.NewTable()
	L.SetField(tbl, "info", L.NewFunction(makeLogFunc(logger, slog.LevelInfo)))
	L.SetField(tbl, "debug", L.NewFunction(makeLogFunc(logger, slog.LevelDebug)))
	L.SetField(tbl, "warn", L.NewFunction(makeLogFunc(logger, slog.LevelWarn)))
	L.SetGlobal("log", tbl)
}

// makeLogFunc returns a Lua function that logs at the given level.
// Signature: log.info(msg, key1, val1, ...) — variadic key-value pairs after message.
func makeLogFunc(logger *slog.Logger, level slog.Level) lua.LGFunction {
	return func(L *lua.LState) int {
		msg := L.CheckString(1)

		// Collect variadic key-value pairs.
		top := L.GetTop()
		var attrs []any
		for i := 2; i <= top-1; i += 2 {
			key := L.ToStringMeta(L.Get(i)).String()
			val := luaToGoValue(L.Get(i + 1))
			attrs = append(attrs, key, val)
		}
		// If odd number of extra args, append the last key with nil value.
		if (top-1)%2 == 1 && top > 1 {
			key := L.ToStringMeta(L.Get(top)).String()
			attrs = append(attrs, key, nil)
		}

		if logger != nil {
			logger.Log(nil, level, msg, attrs...) //nolint:sloglint
		}
		return 0
	}
}

// luaToGoValue converts a Lua value to a Go interface{} for slog.
func luaToGoValue(v lua.LValue) any {
	switch val := v.(type) {
	case lua.LBool:
		return bool(val)
	case lua.LNumber:
		return float64(val)
	case lua.LString:
		return string(val)
	case *lua.LNilType:
		return nil
	default:
		return v.String()
	}
}
