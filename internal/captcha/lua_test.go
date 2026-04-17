package captcha

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
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

			-- PoW (difficulty=1 = 1 leading hex zero, fast)
			local hash = crypto.pow_solve("test", 1)
			if hash == "" then error("pow failed") end
			if #hash ~= 64 then error("pow hash length: " .. #hash) end

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

func TestLuaMod_Utils(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			-- Regex
			local m = re.match("token=([^&]+)", "foo=bar&token=abc123&x=1")
			if m == nil then error("re.match nil") end
			if m[1] ~= "abc123" then error("re.match[1]: " .. tostring(m[1])) end

			local all = re.find_all("\\d+", "a1b23c456")
			if #all ~= 3 then error("re.find_all: " .. #all) end

			local r = re.replace("world", "hello world", "lua")
			if r ~= "hello lua" then error("re.replace: " .. r) end

			-- URL
			local enc = url.encode("hello world&foo")
			if not string.find(enc, "hello") then error("url.encode") end
			local dec = url.decode(enc)
			if dec ~= "hello world&foo" then error("url.decode: " .. dec) end

			local p = url.parse("https://api.vk.com/method/test?v=5.131&key=val")
			if p.host ~= "api.vk.com" then error("url.parse host: " .. tostring(p.host)) end
			if p.raw_query.v ~= "5.131" then error("url.parse query v") end

			-- Time
			local now = time.now()
			if now < 1000000000000 then error("time.now") end

			-- Log (no crash)
			log.info("test", "k", "v")
			log.debug("dbg")
			log.warn("wrn", "code", 42)

			return "utils-ok"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "utils-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}

func TestLuaMod_FP(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			local points = fp.mouse_path(100, 300, 400, 300, 20)
			if #points < 10 then error("path too short: " .. #points) end
			if points[1].x == nil then error("missing x") end

			local dev = fp.device_info(nil)
			if dev.screenWidth == nil then error("no screenWidth") end
			if dev.language ~= "ru" then error("language: " .. tostring(dev.language)) end

			local hash = fp.browser_fp(nil)
			if #hash ~= 32 then error("fp length: " .. #hash) end

			local rtt = fp.connection_rtt(5)
			if #rtt ~= 5 then error("rtt count: " .. #rtt) end

			local dl = fp.connection_downlink(3)
			if #dl ~= 3 then error("dl count: " .. #dl) end

			local adfp = fp.random_adfp()
			if #adfp ~= 21 then error("adfp length: " .. #adfp) end

			return "fp-ok"
		end
	`))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "fp-ok" {
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

func TestLuaMod_Config(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			if config == nil then error("config nil") end
			-- config should be a table even if empty
			if type(config) ~= "table" then error("config not table") end
			return "config-ok"
		end
	`))
	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "config-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}

func TestLuaMod_Native(t *testing.T) {
	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(`
		function solve(challenge)
			if native == nil then error("native nil") end
			if native.solve_slider == nil then error("solve_slider nil") end
			return "native-ok"
		end
	`))
	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "native-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}

func TestLuaMod_Img(t *testing.T) {
	// Create test PNG
	testImg := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for x := 0; x < 10; x++ {
		for y := 0; y < 10; y++ {
			testImg.Set(x, y, color.RGBA{uint8(x * 25), uint8(y * 25), 128, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, testImg)
	imgB64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	solver := NewLuaSolver(nil)
	solver.SetScript([]byte(fmt.Sprintf(`
		function solve(challenge)
			local image = img.decode_base64("%s")
			if img.width(image) ~= 10 then error("width: " .. img.width(image)) end
			if img.height(image) ~= 10 then error("height") end

			local r, g, b, a = img.pixel(image, 0, 0)
			if a ~= 255 then error("alpha: " .. a) end
			if r ~= 0 then error("red: " .. r) end

			local cropped = img.crop(image, 2, 2, 5, 5)
			if img.width(cropped) ~= 5 then error("crop w: " .. img.width(cropped)) end
			if img.height(cropped) ~= 5 then error("crop h") end

			local gray = img.grayscale(image)
			if img.width(gray) ~= 10 then error("gray width") end

			local resized = img.resize(image, 20, 20)
			if img.width(resized) ~= 20 then error("resize w") end

			local edges = img.edge_detect(image)
			if img.width(edges) ~= 10 then error("edge w") end

			local encoded = img.encode(image, "png")
			if #encoded < 50 then error("encode too small") end

			return "img-ok"
		end
	`, imgB64)))

	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "img-ok" {
		t.Fatalf("got %s", result.SuccessToken)
	}
}
