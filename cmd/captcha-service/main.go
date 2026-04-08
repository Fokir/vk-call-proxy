package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/call-vpn/call-vpn/internal/captcha"
	"github.com/call-vpn/call-vpn/internal/provider"
)

type solveRequest struct {
	RedirectURI string `json:"redirect_uri"`
}

type solveResponse struct {
	SuccessToken string `json:"success_token,omitempty"`
	Error        string `json:"error,omitempty"`
}

type statsResponse struct {
	Solved        int64   `json:"solved"`
	Failed        int64   `json:"failed"`
	Pending       int64   `json:"pending"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	UptimeS       float64 `json:"uptime_s"`
}

type server struct {
	solver         provider.CaptchaSolver
	sem            chan struct{}
	solveTimeout   time.Duration
	requestTimeout time.Duration
	verbose        bool
	startTime      time.Time

	solved       atomic.Int64
	failed       atomic.Int64
	pending      atomic.Int64
	totalDurNs   atomic.Int64
	totalSolved  atomic.Int64 // for avg calculation (solved only)
}

func main() {
	port := flag.Int("port", 8090, "HTTP listen port")
	maxConcurrent := flag.Int("max-concurrent", 1, "max concurrent Chrome instances")
	solveTimeout := flag.Duration("solve-timeout", 30*time.Second, "timeout for Chrome captcha solving")
	requestTimeout := flag.Duration("request-timeout", 2*time.Minute, "total request timeout including queue wait")
	verbose := flag.Bool("verbose", false, "verbose logging")
	noHeadless := flag.Bool("no-headless", false, "show Chrome window (for debugging)")
	flag.Parse()

	if *noHeadless {
		captcha.Headless = false
	}

	s := &server{
		solver: captcha.NewChainSolver(
			captcha.NewDirectSolver(),
			captcha.NewChromedpSolver(),
		),
		sem:            make(chan struct{}, *maxConcurrent),
		solveTimeout:   *solveTimeout,
		requestTimeout: *requestTimeout,
		verbose:        *verbose,
		startTime:      time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /solve", s.handleSolve)
	mux.HandleFunc("GET /stats", s.handleStats)

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
	}

	// Graceful shutdown.
	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("captcha-service listening on :%d (max-concurrent=%d)", *port, *maxConcurrent)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-done
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
	log.Println("stopped")
}

func (s *server) handleSolve(w http.ResponseWriter, r *http.Request) {
	// Total request timeout (includes queue wait + solve).
	ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
	defer cancel()

	var req solveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, solveResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	if req.RedirectURI == "" {
		writeJSON(w, http.StatusBadRequest, solveResponse{Error: "redirect_uri is required"})
		return
	}

	s.pending.Add(1)
	defer s.pending.Add(-1)

	if s.verbose {
		log.Printf("queued: %s", req.RedirectURI)
	}

	// Acquire semaphore (FIFO via channel).
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		s.failed.Add(1)
		writeJSON(w, http.StatusGatewayTimeout, solveResponse{Error: "queue timeout"})
		return
	}
	defer func() { <-s.sem }()

	// Solve with its own timeout.
	solveCtx, solveCancel := context.WithTimeout(ctx, s.solveTimeout)
	defer solveCancel()

	start := time.Now()
	result, err := s.solver.SolveCaptcha(solveCtx, &provider.CaptchaChallenge{RedirectURI: req.RedirectURI})
	dur := time.Since(start)

	if err != nil {
		s.failed.Add(1)
		log.Printf("solve failed (%v): %v", dur.Round(time.Millisecond), err)
		writeJSON(w, http.StatusInternalServerError, solveResponse{Error: err.Error()})
		return
	}

	s.solved.Add(1)
	s.totalSolved.Add(1)
	s.totalDurNs.Add(dur.Nanoseconds())

	if s.verbose {
		log.Printf("solved (%v): %s…", dur.Round(time.Millisecond), result.SuccessToken[:min(16, len(result.SuccessToken))])
	}

	writeJSON(w, http.StatusOK, solveResponse{SuccessToken: result.SuccessToken})
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	solved := s.totalSolved.Load()
	var avgMs float64
	if solved > 0 {
		avgMs = float64(s.totalDurNs.Load()) / float64(solved) / 1e6
	}
	writeJSON(w, http.StatusOK, statsResponse{
		Solved:        s.solved.Load(),
		Failed:        s.failed.Load(),
		Pending:       s.pending.Load(),
		AvgDurationMs: avgMs,
		UptimeS:       time.Since(s.startTime).Seconds(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
