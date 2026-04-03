package captcha_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/call-vpn/call-vpn/internal/captcha"
	"github.com/call-vpn/call-vpn/internal/provider"
)

// TestRemoteSolver_Integration starts an in-process captcha-service,
// sends a solve request through RemoteSolver, and verifies the result.
//
// Set CAPTCHA_TEST_URI to a real VK captcha redirect_uri to test with Chrome.
// Without it, only the /stats endpoint and error handling are tested.
func TestRemoteSolver_Integration(t *testing.T) {
	// Start in-process captcha-service.
	srv := newTestCaptchaService(t)
	defer srv.Close()

	// Test /stats returns valid JSON with zero counters.
	t.Run("stats", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/stats")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		var stats struct {
			Solved  int64 `json:"solved"`
			Failed  int64 `json:"failed"`
			Pending int64 `json:"pending"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
			t.Fatal(err)
		}
		if stats.Solved != 0 || stats.Failed != 0 || stats.Pending != 0 {
			t.Fatalf("expected zero counters, got %+v", stats)
		}
	})

	// Test RemoteSolver with missing redirect_uri.
	t.Run("empty_uri", func(t *testing.T) {
		solver := captcha.NewRemoteSolver(srv.URL)
		_, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{})
		if err == nil {
			t.Fatal("expected error for empty redirect_uri")
		}
	})

	// Test RemoteSolver with real captcha (requires CAPTCHA_TEST_URI and Chrome).
	t.Run("real_captcha", func(t *testing.T) {
		uri := os.Getenv("CAPTCHA_TEST_URI")
		if uri == "" {
			t.Skip("CAPTCHA_TEST_URI not set, skipping real captcha test")
		}

		solver := captcha.NewRemoteSolver(srv.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		result, err := solver.SolveCaptcha(ctx, &provider.CaptchaChallenge{
			RedirectURI: uri,
		})
		if err != nil {
			t.Fatalf("solve failed: %v", err)
		}
		if result.SuccessToken == "" {
			t.Fatal("empty success_token")
		}
		t.Logf("success_token: %s...", result.SuccessToken[:min(32, len(result.SuccessToken))])

		// Verify stats updated.
		resp, err := http.Get(srv.URL + "/stats")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var stats struct {
			Solved int64 `json:"solved"`
		}
		json.NewDecoder(resp.Body).Decode(&stats)
		if stats.Solved != 1 {
			t.Fatalf("expected solved=1, got %d", stats.Solved)
		}
	})
}

// newTestCaptchaService starts an httptest.Server that mimics cmd/captcha-service.
func newTestCaptchaService(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	type solveReq struct {
		RedirectURI string `json:"redirect_uri"`
	}
	type solveResp struct {
		SuccessToken string `json:"success_token,omitempty"`
		Error        string `json:"error,omitempty"`
	}
	type statsResp struct {
		Solved  int64   `json:"solved"`
		Failed  int64   `json:"failed"`
		Pending int64   `json:"pending"`
		UptimeS float64 `json:"uptime_s"`
	}

	var solved, failed int64
	start := time.Now()

	mux.HandleFunc("/solve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req solveReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(solveResp{Error: "invalid JSON"})
			return
		}
		if req.RedirectURI == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(solveResp{Error: "redirect_uri is required"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		token, err := captcha.SolveCaptchaURI(ctx, req.RedirectURI)
		if err != nil {
			failed++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(solveResp{Error: err.Error()})
			return
		}

		solved++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(solveResp{SuccessToken: token})
	})

	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(statsResp{
			Solved:  solved,
			Failed:  failed,
			UptimeS: time.Since(start).Seconds(),
		})
	})

	return httptest.NewServer(mux)
}

func TestBrowserSolver_ServerStarts(t *testing.T) {
	// Verify BrowserSolver can be created without panic.
	solver := captcha.NewBrowserSolver()
	if solver == nil {
		t.Fatal("NewBrowserSolver returned nil")
	}

	// Test with cancelled context — should return immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := solver.SolveCaptcha(ctx, &provider.CaptchaChallenge{
		RedirectURI: "https://example.com",
	})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestChromedpSolver_NoChrome(t *testing.T) {
	// If Chrome is not installed, SolveCaptcha should return an error (not panic).
	// With very short timeout to avoid hanging.
	solver := captcha.NewChromedpSolver()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := solver.SolveCaptcha(ctx, &provider.CaptchaChallenge{
		RedirectURI: "https://example.com/not-a-real-captcha",
	})
	// Either Chrome not found error or timeout — both are acceptable.
	if err == nil {
		t.Fatal("expected error")
	}
	t.Logf("expected error: %v", err)
}

// Verify RemoteSolver sends correct JSON to the server.
func TestRemoteSolver_RequestFormat(t *testing.T) {
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success_token":"test-token-123"}`)
	}))
	defer ts.Close()

	solver := captcha.NewRemoteSolver(ts.URL)
	result, err := solver.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{
		RedirectURI: "https://id.vk.com/not_robot_captcha?session_token=abc",
		CaptchaSID:  "12345",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SuccessToken != "test-token-123" {
		t.Fatalf("expected test-token-123, got %s", result.SuccessToken)
	}

	// Verify request body contains redirect_uri.
	var req struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatal(err)
	}
	if req.RedirectURI != "https://id.vk.com/not_robot_captcha?session_token=abc" {
		t.Fatalf("unexpected redirect_uri: %s", req.RedirectURI)
	}
}
