package luamod

import (
	"context"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// RegisterTime registers the `time` global table into L.
func RegisterTime(L *lua.LState, ctx context.Context) {
	tbl := L.NewTable()
	L.SetField(tbl, "sleep", L.NewFunction(makeTimeSleep(ctx)))
	L.SetField(tbl, "now", L.NewFunction(timeNow))
	L.SetField(tbl, "since", L.NewFunction(timeSince))
	L.SetGlobal("time", tbl)
}

// makeTimeSleep returns a Lua function that sleeps for ms milliseconds,
// respecting context cancellation.
func makeTimeSleep(ctx context.Context) lua.LGFunction {
	return func(L *lua.LState) int {
		ms := L.CheckInt(1)
		if ms <= 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			L.RaiseError("time.sleep: context cancelled: %v", ctx.Err())
		case <-time.After(time.Duration(ms) * time.Millisecond):
		}
		return 0
	}
}

// timeNow implements time.now() → Unix milliseconds (int).
func timeNow(L *lua.LState) int {
	ms := time.Now().UnixMilli()
	L.Push(lua.LNumber(ms))
	return 1
}

// timeSince implements time.since(start_ms) → milliseconds elapsed since start_ms.
func timeSince(L *lua.LState) int {
	startMS := L.CheckNumber(1)
	nowMS := time.Now().UnixMilli()
	elapsed := nowMS - int64(startMS)
	L.Push(lua.LNumber(elapsed))
	return 1
}
