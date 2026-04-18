package vk

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/turn"
	lua "github.com/yuin/gopher-lua"
)

func TestVK_ServiceName(t *testing.T) {
	svc := NewService("testlink")
	if got := svc.Name(); got != "vk" {
		t.Errorf("Name() = %q, want %q", got, "vk")
	}
}

func TestVK_IsRateLimitError(t *testing.T) {
	rle := &provider.RateLimitError{Code: 6, Message: "Too many requests"}
	wrapped := fmt.Errorf("step2: %w", rle)

	got, ok := provider.IsRateLimitError(wrapped)
	if !ok {
		t.Fatal("expected IsRateLimitError to return true for wrapped RateLimitError")
	}
	if got.Code != 6 {
		t.Errorf("Code = %d, want 6", got.Code)
	}

	_, ok = provider.IsRateLimitError(fmt.Errorf("some other error"))
	if ok {
		t.Error("expected IsRateLimitError to return false for regular error")
	}
}

func TestVK_ParseTURNURL(t *testing.T) {
	tests := []struct {
		url      string
		wantHost string
		wantPort string
	}{
		{"turn:155.212.199.165:19302", "155.212.199.165", "19302"},
		{"turn:10.0.0.1:3478?transport=tcp", "10.0.0.1", "3478"},
		{"turns:example.com:443", "example.com", "443"},
		{"turn:hostname", "hostname", "3478"},
	}

	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			host, port := turn.ParseTURNURL(tt.url)
			if host != tt.wantHost || port != tt.wantPort {
				t.Errorf("ParseTURNURL(%q) = (%q, %q), want (%q, %q)",
					tt.url, host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestVK_OKJoinConference_ParsesCredentials(t *testing.T) {
	// Test the same JSON structure that auth.lua parses via okJoinConference.
	respBody := `{
		"turn_server": {
			"username": "testuser",
			"credential": "testpass",
			"urls": ["turn:10.0.0.1:3478", "turn:10.0.0.2:19302?transport=tcp"]
		},
		"endpoint": "wss://signal.example.com/ws",
		"id": "conv-123",
		"device_idx": 42
	}`

	var resp struct {
		TurnServer struct {
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
			URLs       []string `json:"urls"`
		} `json:"turn_server"`
		Endpoint  string `json:"endpoint"`
		ID        string `json:"id"`
		DeviceIdx int    `json:"device_idx"`
	}

	if err := json.Unmarshal([]byte(respBody), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.TurnServer.Username != "testuser" {
		t.Errorf("username = %q, want %q", resp.TurnServer.Username, "testuser")
	}
	if len(resp.TurnServer.URLs) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(resp.TurnServer.URLs))
	}

	host, port := turn.ParseTURNURL(resp.TurnServer.URLs[0])
	if host != "10.0.0.1" || port != "3478" {
		t.Errorf("first URL parsed as (%q, %q), want (10.0.0.1, 3478)", host, port)
	}
}

func TestVK_ClassifyLuaError(t *testing.T) {
	tests := []struct {
		name     string
		errMsg   string
		wantRate bool
		wantCode int
	}{
		{"rate limit code 14", "rate_limit:14:Captcha needed", true, 14},
		{"rate limit code 6", "rate_limit:6:Too many requests", true, 6},
		{"rate limit code 1105", "rate_limit:1105:Auth flood", true, 1105},
		{"normal error", "vkJoinToken: VK error 100: bad params", false, 0},
		{"network error", "http request failed: connection refused", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyLuaError(fmt.Errorf("%s", tt.errMsg))
			rle, ok := provider.IsRateLimitError(err)
			if tt.wantRate {
				if !ok {
					t.Fatalf("expected RateLimitError, got: %v", err)
				}
				if rle.Code != tt.wantCode {
					t.Errorf("Code = %d, want %d", rle.Code, tt.wantCode)
				}
			} else {
				if ok {
					t.Errorf("expected non-RateLimitError, got RateLimitError{Code: %d}", rle.Code)
				}
			}
		})
	}
}

func TestVK_ParseLuaJoinInfo(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	tbl := L.NewTable()
	tbl.RawSetString("username", lua.LString("user1"))
	tbl.RawSetString("password", lua.LString("pass1"))
	tbl.RawSetString("ws_endpoint", lua.LString("wss://example.com/ws"))
	tbl.RawSetString("conv_id", lua.LString("conv-42"))
	tbl.RawSetString("device_idx", lua.LNumber(7))

	urls := L.NewTable()
	urls.Append(lua.LString("turn:10.0.0.1:3478"))
	urls.Append(lua.LString("turn:10.0.0.2:19302?transport=tcp"))
	tbl.RawSetString("urls", urls)

	info, err := parseLuaJoinInfo(tbl)
	if err != nil {
		t.Fatalf("parseLuaJoinInfo: %v", err)
	}

	if info.Credentials.Username != "user1" {
		t.Errorf("Username = %q, want %q", info.Credentials.Username, "user1")
	}
	if info.Credentials.Password != "pass1" {
		t.Errorf("Password = %q, want %q", info.Credentials.Password, "pass1")
	}
	if info.Credentials.Host != "10.0.0.1" {
		t.Errorf("Host = %q, want %q", info.Credentials.Host, "10.0.0.1")
	}
	if info.Credentials.Port != "3478" {
		t.Errorf("Port = %q, want %q", info.Credentials.Port, "3478")
	}
	if len(info.Credentials.Servers) != 2 {
		t.Fatalf("Servers count = %d, want 2", len(info.Credentials.Servers))
	}
	if info.Credentials.Servers[1].Host != "10.0.0.2" {
		t.Errorf("Servers[1].Host = %q, want %q", info.Credentials.Servers[1].Host, "10.0.0.2")
	}
	if info.WSEndpoint != "wss://example.com/ws" {
		t.Errorf("WSEndpoint = %q, want %q", info.WSEndpoint, "wss://example.com/ws")
	}
	if info.ConvID != "conv-42" {
		t.Errorf("ConvID = %q, want %q", info.ConvID, "conv-42")
	}
	if info.DeviceIdx != 7 {
		t.Errorf("DeviceIdx = %d, want 7", info.DeviceIdx)
	}
}

func TestVK_ParseLuaJoinInfo_NoURLs(t *testing.T) {
	L := lua.NewState()
	defer L.Close()

	tbl := L.NewTable()
	tbl.RawSetString("username", lua.LString("user1"))

	_, err := parseLuaJoinInfo(tbl)
	if err == nil {
		t.Fatal("expected error for missing URLs")
	}
}
