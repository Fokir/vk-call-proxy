package vk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/turn"
)

func TestVK_ServiceName(t *testing.T) {
	svc := NewService("testlink")
	if got := svc.Name(); got != "vk" {
		t.Errorf("Name() = %q, want %q", got, "vk")
	}
}

func TestVK_CheckVKRateLimit(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		msg      string
		wantRate bool
	}{
		{"code 6 too many requests", 6, "Too many requests per second", true},
		{"code 9 flood control", 9, "Flood control", true},
		{"code 14 captcha required", 14, "Captcha needed", true},
		{"code 29 rate limit", 29, "Rate limit reached", true},
		{"code 5 auth error", 5, "User authorization failed", false},
		{"code 100 param error", 100, "One of the parameters specified was missing", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(vkErrorResponse{
				Error: &struct {
					Code        int     `json:"error_code"`
					Msg         string  `json:"error_msg"`
					RedirectURI string  `json:"redirect_uri"`
					CaptchaSID  string  `json:"captcha_sid"`
					CaptchaTS   float64 `json:"captcha_ts"`
					CaptchaImg  string  `json:"captcha_img"`
				}{Code: tt.code, Msg: tt.msg},
			})

			rle := checkVKRateLimit(body)
			if tt.wantRate {
				if rle == nil {
					t.Fatal("expected RateLimitError, got nil")
				}
				if rle.Code != tt.code {
					t.Errorf("Code = %d, want %d", rle.Code, tt.code)
				}
				if rle.Message != tt.msg {
					t.Errorf("Message = %q, want %q", rle.Message, tt.msg)
				}
			} else {
				if rle != nil {
					t.Errorf("expected nil, got RateLimitError{Code: %d}", rle.Code)
				}
			}
		})
	}
}

func TestVK_CheckVKRateLimit_InvalidJSON(t *testing.T) {
	rle := checkVKRateLimit([]byte(`not json`))
	if rle != nil {
		t.Errorf("expected nil for invalid JSON, got %+v", rle)
	}
}

func TestVK_CheckVKRateLimit_NoError(t *testing.T) {
	rle := checkVKRateLimit([]byte(`{"response":{"token":"abc"}}`))
	if rle != nil {
		t.Errorf("expected nil for no-error response, got %+v", rle)
	}
}

func TestVK_CheckLoginRateLimit(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantRate bool
	}{
		{
			"error code 1105",
			`{"error":"auth_flood","error_code":1105,"error_description":"Auth flood detected"}`,
			true,
		},
		{
			"Too many in description",
			`{"error":"too_many","error_code":0,"error_description":"Too many auth attempts"}`,
			true,
		},
		{
			"normal success response",
			`{"data":{"access_token":"abc123"}}`,
			false,
		},
		{
			"other error",
			`{"error":"invalid_client","error_code":100,"error_description":"Invalid client_id"}`,
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rle := checkLoginRateLimit([]byte(tt.body))
			if tt.wantRate {
				if rle == nil {
					t.Fatal("expected RateLimitError, got nil")
				}
				if rle.Code != 1105 {
					t.Errorf("Code = %d, want 1105", rle.Code)
				}
			} else {
				if rle != nil {
					t.Errorf("expected nil, got RateLimitError{Code: %d}", rle.Code)
				}
			}
		})
	}
}

func TestVK_IsRateLimitError(t *testing.T) {
	// RateLimitError wraps correctly
	rle := &provider.RateLimitError{Code: 6, Message: "Too many requests"}
	wrapped := fmt.Errorf("step2: %w", rle)

	got, ok := provider.IsRateLimitError(wrapped)
	if !ok {
		t.Fatal("expected IsRateLimitError to return true for wrapped RateLimitError")
	}
	if got.Code != 6 {
		t.Errorf("Code = %d, want 6", got.Code)
	}

	// Regular error is not RateLimitError
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

func TestVK_ClassifyToken(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{"vk1.a.abc123", "vk"},
		{"$oktoken123", "ok"},
		{"randomtoken", "unknown"},
		{"", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want+"_"+tt.token[:min(len(tt.token), 10)], func(t *testing.T) {
			if got := classifyToken(tt.token); got != tt.want {
				t.Errorf("classifyToken(%q) = %q, want %q", tt.token, got, tt.want)
			}
		})
	}
}

func TestVK_OKJoinConference_ParsesCredentials(t *testing.T) {
	// Mock server that returns a joinConference-like response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"turn_server": map[string]interface{}{
				"username":   "testuser",
				"credential": "testpass",
				"urls":       []string{"turn:10.0.0.1:3478", "turn:10.0.0.2:19302?transport=tcp"},
			},
			"endpoint":   "wss://signal.example.com/ws",
			"id":         "conv-123",
			"device_idx": 42,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Call okJoinConference with the mock server.
	// We need to override the URL — okJoinConference uses a hardcoded URL,
	// so we test the parsing by calling the function through httpPost + manual parse.
	// Instead, test the response parsing path directly via a helper approach.

	// Since okJoinConference has a hardcoded URL, we test the HTTP response
	// parsing by directly unmarshaling the same structure it uses.
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
	if resp.TurnServer.Credential != "testpass" {
		t.Errorf("credential = %q, want %q", resp.TurnServer.Credential, "testpass")
	}
	if len(resp.TurnServer.URLs) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(resp.TurnServer.URLs))
	}

	// Verify TURN URL parsing produces correct servers
	host, port := turn.ParseTURNURL(resp.TurnServer.URLs[0])
	if host != "10.0.0.1" || port != "3478" {
		t.Errorf("first URL parsed as (%q, %q), want (10.0.0.1, 3478)", host, port)
	}

	var servers []provider.TURNServer
	for _, u := range resp.TurnServer.URLs {
		h, p := turn.ParseTURNURL(u)
		servers = append(servers, provider.TURNServer{Host: h, Port: p})
	}
	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}
	if servers[1].Host != "10.0.0.2" || servers[1].Port != "19302" {
		t.Errorf("second server = %+v, want {Host:10.0.0.2, Port:19302}", servers[1])
	}

	srv.Close() // close early, we didn't actually need it
}

func TestVK_VKAnonToken_RateLimit(t *testing.T) {
	// Mock server that returns a login rate-limit response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"error":             "auth_flood",
			"error_code":        1105,
			"error_description": "Too many auth requests",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// We can't call vkAnonToken directly with a custom URL, but we can
	// test the underlying checkLoginRateLimit with what the server returns.
	client := srv.Client()

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var buf [4096]byte
	n, _ := resp.Body.Read(buf[:])
	body := buf[:n]

	rle := checkLoginRateLimit(body)
	if rle == nil {
		t.Fatal("expected RateLimitError from login flood response")
	}
	if rle.Code != 1105 {
		t.Errorf("Code = %d, want 1105", rle.Code)
	}

	// Verify it satisfies IsRateLimitError
	_, ok := provider.IsRateLimitError(rle)
	if !ok {
		t.Error("expected IsRateLimitError to return true")
	}
}

func TestVK_VKAPIPost_RateLimit(t *testing.T) {
	// Mock VK API that returns a rate-limit error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"error": map[string]interface{}{
				"error_code": 6,
				"error_msg":  "Too many requests per second",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()

	_, err := vkAPIPost(ctx, client, "TestAgent/1.0", srv.URL, nil)
	if err == nil {
		t.Fatal("expected error from rate-limited vkAPIPost")
	}

	rle, ok := provider.IsRateLimitError(err)
	if !ok {
		t.Fatalf("expected RateLimitError, got: %v", err)
	}
	if rle.Code != 6 {
		t.Errorf("Code = %d, want 6", rle.Code)
	}
}

func TestVK_VKAPIPost_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"response":{"token":"abc123"}}`))
	}))
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()

	body, err := vkAPIPost(ctx, client, "TestAgent/1.0", srv.URL, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != `{"response":{"token":"abc123"}}` {
		t.Errorf("unexpected body: %s", string(body))
	}
}

func TestVK_HttpPost_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	client := srv.Client()
	ctx := context.Background()

	_, err := httpPost(ctx, client, "TestAgent/1.0", srv.URL, nil)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if got := err.Error(); got != "HTTP 500: internal server error" {
		t.Errorf("error = %q, want %q", got, "HTTP 500: internal server error")
	}
}
