package captcha

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
)

func TestLuaSolver_SimpleReturn(t *testing.T) {
	s := NewLuaSolver(nil)
	s.SetScript([]byte(`
		function solve(challenge)
			return "test-token-123"
		end
	`))

	res, err := s.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		CaptchaSID: "sid1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SuccessToken != "test-token-123" {
		t.Fatalf("got token %q, want %q", res.SuccessToken, "test-token-123")
	}
}

func TestLuaSolver_ScriptError(t *testing.T) {
	s := NewLuaSolver(nil)
	s.SetScript([]byte(`
		function solve(challenge)
			error("something went wrong")
		end
	`))

	_, err := s.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Fatalf("error should contain script message, got: %v", err)
	}
}

func TestLuaSolver_NoScript(t *testing.T) {
	s := NewLuaSolver(nil) // no manager, no override

	_, err := s.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no solver.lua") {
		t.Fatalf("error should mention missing script, got: %v", err)
	}
}

func TestLuaSolver_Timeout(t *testing.T) {
	s := NewLuaSolver(nil)
	s.SetScript([]byte(`
		function solve(challenge)
			while true do end
		end
	`))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := s.SolveCaptcha(ctx, &provider.CaptchaChallenge{})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Fatalf("error should mention context, got: %v", err)
	}
}

func TestLuaMod_JSON(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			local t = json.decode('{"status":"OK","token":"abc","nums":[1,2,3]}')
			if t.status ~= "OK" then error("decode status") end
			if t.nums[2] ~= 2 then error("decode array") end
			local s = json.encode({result = t.token, count = 42})
			if not string.find(s, "abc") then error("encode missing abc") end
			return t.token
		end
	`))
	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "abc" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}

func TestLuaMod_Crypto(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			-- SHA-256
			local h = crypto.sha256("hello")
			if h ~= "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" then
				error("sha256: " .. h)
			end

			-- MD5
			local m = crypto.md5("hello")
			if m ~= "5d41402abc4b2a76b9719d911017c592" then error("md5: " .. m) end

			-- Base64 roundtrip
			local enc = crypto.base64_encode("hello world")
			local dec = crypto.base64_decode(enc)
			if dec ~= "hello world" then error("base64 roundtrip") end

			-- PoW (difficulty 4 bits = 1 hex zero, should be fast)
			local nonce = crypto.pow_solve("test", 4)
			if nonce == "" then error("pow failed") end

			-- Random bytes
			local r = crypto.random_bytes(16)
			if #r ~= 16 then error("random_bytes length: " .. #r) end

			-- XOR
			local x = crypto.xor("ab", "cd")
			if #x ~= 2 then error("xor length") end

			return "crypto-ok"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "crypto-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}

func TestLuaMod_HTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/get":
			w.Header().Set("X-Test", "hello")
			fmt.Fprintf(w, `{"method":"GET","ua":"%s"}`, r.Header.Get("User-Agent"))
		case "/form":
			r.ParseForm()
			fmt.Fprintf(w, `{"f1":"%s","f2":"%s"}`, r.FormValue("f1"), r.FormValue("f2"))
		case "/json":
			body, _ := io.ReadAll(r.Body)
			w.Write(body)
		}
	}))
	defer ts.Close()

	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(fmt.Sprintf(`
		function solve(challenge)
			-- GET
			local r = http.get("%s/get", {["User-Agent"] = "TestBot"})
			if r.status ~= 200 then error("status: " .. r.status) end
			local d = json.decode(r.body)
			if d.ua ~= "TestBot" then error("ua: " .. d.ua) end

			-- POST form
			local r2 = http.post_form("%s/form", {}, {f1 = "hello", f2 = "world"})
			local d2 = json.decode(r2.body)
			if d2.f1 ~= "hello" then error("f1: " .. tostring(d2.f1)) end

			-- POST JSON
			local r3 = http.post_json("%s/json", {}, {key = "val"})
			local d3 = json.decode(r3.body)
			if d3.key ~= "val" then error("key: " .. tostring(d3.key)) end

			return "http-ok"
		end
	`, ts.URL, ts.URL, ts.URL)))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "http-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}

func TestLuaSolver_ChallengeFields(t *testing.T) {
	s := NewLuaSolver(nil)
	s.SetScript([]byte(`
		function solve(challenge)
			if challenge.redirect_uri ~= "https://example.com/captcha" then
				error("bad redirect_uri: " .. tostring(challenge.redirect_uri))
			end
			if challenge.captcha_sid ~= "sid-42" then
				error("bad captcha_sid: " .. tostring(challenge.captcha_sid))
			end
			if challenge.captcha_ts ~= 1234567890.5 then
				error("bad captcha_ts: " .. tostring(challenge.captcha_ts))
			end
			return "fields-ok"
		end
	`))

	res, err := s.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		RedirectURI: "https://example.com/captcha",
		CaptchaSID:  "sid-42",
		CaptchaTS:   1234567890.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SuccessToken != "fields-ok" {
		t.Fatalf("got token %q, want %q", res.SuccessToken, "fields-ok")
	}
}
