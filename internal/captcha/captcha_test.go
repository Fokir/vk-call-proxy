package captcha

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

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
		solver := NewRemoteSolver(srv.URL)
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

		solver := NewRemoteSolver(srv.URL)
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

		token, err := SolveCaptchaURI(ctx, req.RedirectURI)
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
	solver := NewBrowserSolver()
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

func TestSliderSolver(t *testing.T) {
	// Simulated getContent response with a known-solvable puzzle.
	// Uses a 3x3 grid with swaps that produce identity at position 2.
	//
	// Create a simple 30x30 gradient image (3x3 tiles, 10px each).
	const gridSize = 3
	const tileSize = 10
	imgSize := gridSize * tileSize

	img := image.NewRGBA(image.Rect(0, 0, imgSize, imgSize))
	for y := 0; y < imgSize; y++ {
		for x := 0; x < imgSize; x++ {
			// Gradient: each tile has a distinct color based on position.
			tileR := y / tileSize
			tileC := x / tileSize
			r := uint8(tileR*80 + tileC*20)
			g := uint8(tileC*80 + tileR*20)
			b := uint8(50)
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}

	// Encode to JPEG.
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	imgB64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Create steps: gridSize=3, then swap pairs.
	// We'll swap tiles 0↔4 and then 0↔4 again (undo) at position 2 → identity.
	// Position 0: identity (no swaps)
	// Position 1: swap(0,4) → [4,1,2,3,0,5,6,7,8]
	// Position 2: swap(0,4) again → back to identity
	// The image is a gradient, so identity has best edge score.
	steps := []int{3, 0, 4, 0, 4} // gridSize=3, pairs: (0,4), (0,4)
	// 4 remaining elements, even → no attempts field.

	resp := fmt.Sprintf(`{"response":{"status":"OK","extension":"jpeg","image":"%s","steps":[%d,%d,%d,%d,%d]}}`,
		imgB64, steps[0], steps[1], steps[2], steps[3], steps[4])

	puzzle, err := parseSliderContent([]byte(resp))
	if err != nil {
		t.Fatal(err)
	}

	if puzzle.gridSize != 3 {
		t.Fatalf("expected gridSize=3, got %d", puzzle.gridSize)
	}
	if len(puzzle.swapPairs) != 4 {
		t.Fatalf("expected 4 swap pairs elements, got %d", len(puzzle.swapPairs))
	}

	answer, err := solveSlider(puzzle)
	if err != nil {
		t.Fatal(err)
	}

	// Position 0 (identity) should have best score since image is a smooth gradient.
	if len(answer) != 0 {
		t.Fatalf("expected answer length 0 (position 0 = identity), got %d", len(answer))
	}

	// Test encodeSliderAnswer.
	encoded := encodeSliderAnswer(answer)
	if encoded == "" {
		t.Fatal("empty encoded answer")
	}
	t.Logf("encoded answer: %s", encoded)
}

func TestSliderSolver_NonTrivial(t *testing.T) {
	// Test with a scrambled image where the correct position is NOT 0.
	const gridSize = 3
	const tileSize = 20
	imgSize := gridSize * tileSize

	// Create image where tiles are pre-scrambled: tile at position (0,0) contains
	// what should be at (1,1), etc. The solver needs to find swaps that unscramble it.
	//
	// Original correct image: horizontal gradient R=col*80, G=row*80.
	// Scramble: swap source tiles 0↔4 in the image.
	// Then the solver should find that applying swap(0,4) produces best edges.

	img := image.NewRGBA(image.Rect(0, 0, imgSize, imgSize))

	// First, create the "correct" tiles.
	correctTiles := make([]*image.RGBA, gridSize*gridSize)
	for idx := 0; idx < gridSize*gridSize; idx++ {
		tr := idx / gridSize
		tc := idx % gridSize
		tile := image.NewRGBA(image.Rect(0, 0, tileSize, tileSize))
		for y := 0; y < tileSize; y++ {
			for x := 0; x < tileSize; x++ {
				// Smooth gradient across the whole image.
				globalX := tc*tileSize + x
				globalY := tr*tileSize + y
				r := uint8(globalX * 255 / imgSize)
				g := uint8(globalY * 255 / imgSize)
				tile.SetRGBA(x, y, color.RGBA{R: r, G: g, B: 100, A: 255})
			}
		}
		correctTiles[idx] = tile
	}

	// Scramble: swap tiles 0 and 4 in the image.
	scrambled := make([]*image.RGBA, gridSize*gridSize)
	copy(scrambled, correctTiles)
	scrambled[0], scrambled[4] = scrambled[4], scrambled[0]

	// Write scrambled tiles into image.
	for idx, tile := range scrambled {
		tr := idx / gridSize
		tc := idx % gridSize
		for y := 0; y < tileSize; y++ {
			for x := 0; x < tileSize; x++ {
				img.SetRGBA(tc*tileSize+x, tr*tileSize+y, tile.RGBAAt(x, y))
			}
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	imgB64 := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Steps: swap(0,4) at position 1 → unscrambles the image.
	// Add more swaps after to ensure solver picks position 1.
	steps := []int{3, 0, 4, 2, 7, 1, 5}
	// After removing gridSize=3: [0, 4, 2, 7, 1, 5] → 6 elements, 3 positions.
	// Position 0: identity (scrambled image shown as-is)
	// Position 1: swap(0,4) → unscrambled!
	// Position 2: swap(2,7) → scrambled again.

	stepsJSON := "["
	for i, s := range steps {
		if i > 0 {
			stepsJSON += ","
		}
		stepsJSON += fmt.Sprintf("%d", s)
	}
	stepsJSON += "]"

	resp := fmt.Sprintf(`{"response":{"status":"OK","extension":"jpeg","image":"%s","steps":%s}}`,
		imgB64, stepsJSON)

	puzzle, err := parseSliderContent([]byte(resp))
	if err != nil {
		t.Fatal(err)
	}

	answer, err := solveSlider(puzzle)
	if err != nil {
		t.Fatal(err)
	}

	// Should be position 1: swap(0,4) → answer = [0, 4].
	if len(answer) != 2 || answer[0] != 0 || answer[1] != 4 {
		t.Fatalf("expected answer [0,4] (position 1), got %v", answer)
	}
	t.Logf("correctly found position 1, answer: %v", answer)
}

func TestChromedpSolver_NoChrome(t *testing.T) {
	// If Chrome is not installed, SolveCaptcha should return an error (not panic).
	// With very short timeout to avoid hanging.
	solver := NewChromedpSolver()
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

	solver := NewRemoteSolver(ts.URL)
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

func TestChainSolver(t *testing.T) {
	errFail := fmt.Errorf("solver failed")

	failing := &mockSolver{err: errFail}
	notSlider := &mockSolver{err: fmt.Errorf("%w: no slider", ErrNotSlider)}
	succeeding := &mockSolver{result: &provider.CaptchaResult{SuccessToken: "token123"}}

	t.Run("first succeeds", func(t *testing.T) {
		chain := NewChainSolver(succeeding, failing)
		res, err := chain.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{RedirectURI: "https://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if res.SuccessToken != "token123" {
			t.Fatalf("expected token123, got %s", res.SuccessToken)
		}
	})

	t.Run("first fails then second succeeds", func(t *testing.T) {
		chain := NewChainSolver(notSlider, succeeding)
		res, err := chain.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{RedirectURI: "https://example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if res.SuccessToken != "token123" {
			t.Fatalf("expected token123, got %s", res.SuccessToken)
		}
	})

	t.Run("all fail returns last error", func(t *testing.T) {
		chain := NewChainSolver(notSlider, failing)
		_, err := chain.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{RedirectURI: "https://example.com"})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("empty chain", func(t *testing.T) {
		chain := NewChainSolver()
		_, err := chain.SolveCaptcha(context.Background(), &provider.CaptchaChallenge{RedirectURI: "https://example.com"})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

type mockSolver struct {
	result *provider.CaptchaResult
	err    error
}

func (m *mockSolver) SolveCaptcha(_ context.Context, _ *provider.CaptchaChallenge) (*provider.CaptchaResult, error) {
	return m.result, m.err
}
