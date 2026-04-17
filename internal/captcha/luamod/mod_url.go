package luamod

import (
	"net/url"

	lua "github.com/yuin/gopher-lua"
)

// RegisterURL registers the `url` global table into L.
func RegisterURL(L *lua.LState) {
	tbl := L.NewTable()
	L.SetField(tbl, "encode", L.NewFunction(urlEncode))
	L.SetField(tbl, "decode", L.NewFunction(urlDecode))
	L.SetField(tbl, "parse", L.NewFunction(urlParse))
	L.SetGlobal("url", tbl)
}

// urlEncode implements url.encode(str) → percent-encoded string.
func urlEncode(L *lua.LState) int {
	str := L.CheckString(1)
	L.Push(lua.LString(url.QueryEscape(str)))
	return 1
}

// urlDecode implements url.decode(str) → decoded string.
func urlDecode(L *lua.LState) int {
	str := L.CheckString(1)
	decoded, err := url.QueryUnescape(str)
	if err != nil {
		L.RaiseError("url.decode: %v", err)
		return 0
	}
	L.Push(lua.LString(decoded))
	return 1
}

// urlParse implements url.parse(str) → table {scheme, host, path, query, fragment, raw_query={key=val,...}}.
func urlParse(L *lua.LState) int {
	str := L.CheckString(1)

	u, err := url.Parse(str)
	if err != nil {
		L.RaiseError("url.parse: %v", err)
		return 0
	}

	tbl := L.NewTable()
	tbl.RawSetString("scheme", lua.LString(u.Scheme))
	tbl.RawSetString("host", lua.LString(u.Host))
	tbl.RawSetString("path", lua.LString(u.Path))
	tbl.RawSetString("query", lua.LString(u.RawQuery))
	tbl.RawSetString("fragment", lua.LString(u.Fragment))

	// raw_query as key=val table (first value per key).
	queryTbl := L.NewTable()
	for k, vals := range u.Query() {
		if len(vals) > 0 {
			queryTbl.RawSetString(k, lua.LString(vals[0]))
		}
	}
	tbl.RawSetString("raw_query", queryTbl)

	L.Push(tbl)
	return 1
}
