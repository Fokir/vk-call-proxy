package luamod

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// RegisterHTTP registers the `http` global table into L.
// A fresh cookie jar is created per registration (= per solve session).
func RegisterHTTP(L *lua.LState, opts Options) {
	jar, _ := cookiejar.New(nil)

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	// Clone client so we can set our own jar without mutating the caller's client.
	sessionClient := &http.Client{
		Transport:     client.Transport,
		CheckRedirect: client.CheckRedirect,
		Timeout:       client.Timeout,
		Jar:           jar,
	}

	ctx := opts.Ctx

	doRequest := func(req *http.Request) (lua.LValue, string) {
		if ctx != nil {
			req = req.WithContext(ctx)
		}
		resp, err := sessionClient.Do(req)
		if err != nil {
			return lua.LNil, fmt.Sprintf("http request failed: %v", err)
		}
		defer resp.Body.Close()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return lua.LNil, fmt.Sprintf("http read body: %v", err)
		}

		tbl := L.NewTable()
		L.SetField(tbl, "status", lua.LNumber(resp.StatusCode))
		L.SetField(tbl, "body", lua.LString(bodyBytes))

		headers := L.NewTable()
		for k, vs := range resp.Header {
			L.SetField(headers, k, lua.LString(strings.Join(vs, ", ")))
		}
		L.SetField(tbl, "headers", headers)

		return tbl, ""
	}

	// applyHeaders copies Lua headers table (optional, may be LNil) into the request.
	applyHeaders := func(req *http.Request, hdrs lua.LValue) {
		if tbl, ok := hdrs.(*lua.LTable); ok {
			tbl.ForEach(func(k lua.LValue, v lua.LValue) {
				req.Header.Set(k.String(), v.String())
			})
		}
	}

	mod := L.NewTable()

	// http.get(url [, headers_table]) → resp_table  |  nil, err_string
	L.SetField(mod, "get", L.NewFunction(func(L *lua.LState) int {
		rawURL := L.CheckString(1)
		hdrs := L.OptTable(2, nil)

		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("http.get: bad url: %v", err)))
			return 2
		}
		if hdrs != nil {
			applyHeaders(req, hdrs)
		}

		result, errStr := doRequest(req)
		if errStr != "" {
			L.Push(lua.LNil)
			L.Push(lua.LString(errStr))
			return 2
		}
		L.Push(result)
		return 1
	}))

	// http.post(url [, headers_table [, body_string]]) → resp_table  |  nil, err_string
	L.SetField(mod, "post", L.NewFunction(func(L *lua.LState) int {
		rawURL := L.CheckString(1)
		hdrs := L.OptTable(2, nil)
		body := L.OptString(3, "")

		req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(body))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("http.post: bad url: %v", err)))
			return 2
		}
		if hdrs != nil {
			applyHeaders(req, hdrs)
		}

		result, errStr := doRequest(req)
		if errStr != "" {
			L.Push(lua.LNil)
			L.Push(lua.LString(errStr))
			return 2
		}
		L.Push(result)
		return 1
	}))

	// http.post_form(url [, headers_table [, fields_table]]) → resp_table  |  nil, err_string
	L.SetField(mod, "post_form", L.NewFunction(func(L *lua.LState) int {
		rawURL := L.CheckString(1)
		hdrs := L.OptTable(2, nil)
		fields := L.OptTable(3, nil)

		form := url.Values{}
		if fields != nil {
			fields.ForEach(func(k lua.LValue, v lua.LValue) {
				form.Set(k.String(), v.String())
			})
		}

		req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("http.post_form: bad url: %v", err)))
			return 2
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if hdrs != nil {
			applyHeaders(req, hdrs)
		}

		result, errStr := doRequest(req)
		if errStr != "" {
			L.Push(lua.LNil)
			L.Push(lua.LString(errStr))
			return 2
		}
		L.Push(result)
		return 1
	}))

	// http.post_json(url [, headers_table [, data_table]]) → resp_table  |  nil, err_string
	L.SetField(mod, "post_json", L.NewFunction(func(L *lua.LState) int {
		rawURL := L.CheckString(1)
		hdrs := L.OptTable(2, nil)
		data := L.Get(3) // may be LNil if omitted

		var goData interface{}
		if data != lua.LNil {
			goData = LuaToGo(data)
		}

		jsonBytes, err := json.Marshal(goData)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("http.post_json: marshal: %v", err)))
			return 2
		}

		req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(jsonBytes))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("http.post_json: bad url: %v", err)))
			return 2
		}
		req.Header.Set("Content-Type", "application/json")
		if hdrs != nil {
			applyHeaders(req, hdrs)
		}

		result, errStr := doRequest(req)
		if errStr != "" {
			L.Push(lua.LNil)
			L.Push(lua.LString(errStr))
			return 2
		}
		L.Push(result)
		return 1
	}))

	L.SetGlobal("http", mod)
}
