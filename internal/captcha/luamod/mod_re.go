package luamod

import (
	"regexp"

	lua "github.com/yuin/gopher-lua"
)

// RegisterRe registers the `re` global table into L.
func RegisterRe(L *lua.LState) {
	tbl := L.NewTable()
	L.SetField(tbl, "match", L.NewFunction(reMatch))
	L.SetField(tbl, "find_all", L.NewFunction(reFindAll))
	L.SetField(tbl, "replace", L.NewFunction(reReplace))
	L.SetGlobal("re", tbl)
}

// reMatch implements re.match(pattern, str) → table of captures or nil.
// Index 0 = full match, 1+ = capture groups.
func reMatch(L *lua.LState) int {
	pattern := L.CheckString(1)
	str := L.CheckString(2)

	re, err := regexp.Compile(pattern)
	if err != nil {
		L.RaiseError("re.match: invalid pattern: %v", err)
		return 0
	}

	m := re.FindStringSubmatch(str)
	if m == nil {
		L.Push(lua.LNil)
		return 1
	}

	tbl := L.NewTable()
	for i, s := range m {
		tbl.RawSetInt(i, lua.LString(s))
	}
	L.Push(tbl)
	return 1
}

// reFindAll implements re.find_all(pattern, str) → table of tables (all matches).
func reFindAll(L *lua.LState) int {
	pattern := L.CheckString(1)
	str := L.CheckString(2)

	re, err := regexp.Compile(pattern)
	if err != nil {
		L.RaiseError("re.find_all: invalid pattern: %v", err)
		return 0
	}

	all := re.FindAllStringSubmatch(str, -1)
	result := L.NewTable()
	for i, m := range all {
		inner := L.NewTable()
		for j, s := range m {
			inner.RawSetInt(j, lua.LString(s))
		}
		result.RawSetInt(i+1, inner)
	}
	L.Push(result)
	return 1
}

// reReplace implements re.replace(pattern, str, replacement) → string.
func reReplace(L *lua.LState) int {
	pattern := L.CheckString(1)
	str := L.CheckString(2)
	replacement := L.CheckString(3)

	re, err := regexp.Compile(pattern)
	if err != nil {
		L.RaiseError("re.replace: invalid pattern: %v", err)
		return 0
	}

	L.Push(lua.LString(re.ReplaceAllString(str, replacement)))
	return 1
}
