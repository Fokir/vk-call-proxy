package luamod

import (
	"encoding/json"
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// RegisterJSON registers the `json` global table into L.
func RegisterJSON(L *lua.LState) {
	tbl := L.NewTable()
	L.SetField(tbl, "decode", L.NewFunction(jsonDecode))
	L.SetField(tbl, "encode", L.NewFunction(jsonEncode))
	L.SetGlobal("json", tbl)
}

// jsonDecode implements json.decode(str) → Lua table.
func jsonDecode(L *lua.LState) int {
	str := L.CheckString(1)
	var val interface{}
	if err := json.Unmarshal([]byte(str), &val); err != nil {
		L.RaiseError("json.decode: %v", err)
		return 0
	}
	L.Push(GoToLua(L, val))
	return 1
}

// jsonEncode implements json.encode(table) → JSON string.
func jsonEncode(L *lua.LState) int {
	val := L.CheckAny(1)
	go_val := LuaToGo(val)
	data, err := json.Marshal(go_val)
	if err != nil {
		L.RaiseError("json.encode: %v", err)
		return 0
	}
	L.Push(lua.LString(data))
	return 1
}

// GoToLua converts a Go value (from json.Unmarshal) to a Lua value.
func GoToLua(L *lua.LState, val interface{}) lua.LValue {
	if val == nil {
		return lua.LNil
	}
	switch v := val.(type) {
	case bool:
		return lua.LBool(v)
	case float64:
		return lua.LNumber(v)
	case string:
		return lua.LString(v)
	case []interface{}:
		tbl := L.NewTable()
		for i, item := range v {
			tbl.RawSetInt(i+1, GoToLua(L, item))
		}
		return tbl
	case map[string]interface{}:
		tbl := L.NewTable()
		for k, item := range v {
			tbl.RawSetString(k, GoToLua(L, item))
		}
		return tbl
	default:
		// Fallback: convert to string via fmt
		return lua.LString(fmt.Sprintf("%v", v))
	}
}

// LuaToGo converts a Lua value to a Go value suitable for json.Marshal.
// Arrays (sequential integer keys 1..N) become Go slices; other tables become Go maps.
func LuaToGo(val lua.LValue) interface{} {
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
		return luaTableToGo(v)
	default:
		return nil
	}
}

// luaTableToGo converts a Lua table to either a []interface{} (array) or map[string]interface{}.
func luaTableToGo(tbl *lua.LTable) interface{} {
	// Detect if it's a sequential array: keys must be integers 1..N with no gaps.
	n := tbl.Len()
	if n > 0 {
		isArray := true
		tbl.ForEach(func(key lua.LValue, _ lua.LValue) {
			if !isArray {
				return
			}
			if k, ok := key.(lua.LNumber); ok {
				idx := int(k)
				if float64(idx) != float64(k) || idx < 1 || idx > n {
					isArray = false
				}
			} else {
				isArray = false
			}
		})
		if isArray {
			arr := make([]interface{}, n)
			for i := 1; i <= n; i++ {
				arr[i-1] = LuaToGo(tbl.RawGetInt(i))
			}
			return arr
		}
	}

	// Otherwise treat as map.
	m := make(map[string]interface{})
	tbl.ForEach(func(key lua.LValue, value lua.LValue) {
		m[key.String()] = LuaToGo(value)
	})
	return m
}
