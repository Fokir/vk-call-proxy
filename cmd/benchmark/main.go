// Command benchmark measures raw DTLS throughput over VK TURN relays.
// Run two instances with --role=sender and --role=receiver using the same
// VK call link.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
)

type config struct {
	callLink string
	role     string // "sender" or "receiver"
	mode     string // "raw"
	sizeMB   int
	runs     int
	numConns int
	useTCP   bool
	token    string
	timeout  time.Duration
}

func main() {
	cfg := &config{}
	flag.StringVar(&cfg.callLink, "link", "", "VK call link ID (required)")
	flag.StringVar(&cfg.role, "role", "", "sender or receiver (required)")
	flag.StringVar(&cfg.mode, "mode", "raw", "test mode: raw")
	flag.IntVar(&cfg.sizeMB, "size", 10, "data size in MB per run")
	flag.IntVar(&cfg.runs, "runs", 3, "number of runs per test")
	flag.IntVar(&cfg.numConns, "conns", 4, "TURN connections for raw-dtls test")
	flag.BoolVar(&cfg.useTCP, "tcp", true, "use TCP for TURN")
	flag.StringVar(&cfg.token, "token", "bench-token-2026", "auth token")
	flag.DurationVar(&cfg.timeout, "timeout", 5*time.Minute, "global timeout")
	flag.Parse()

	if cfg.callLink == "" || cfg.role == "" {
		flag.Usage()
		os.Exit(1)
	}
	if cfg.role != "sender" && cfg.role != "receiver" {
		fmt.Fprintf(os.Stderr, "role must be 'sender' or 'receiver'\n")
		os.Exit(1)
	}

	isSender := cfg.role == "sender"

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	// handle Ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		logger.Info("interrupted, shutting down...")
		cancel()
	}()

	// Create call service and fetch join info for signaling.
	svc := vk.NewService(cfg.callLink)
	logger.Info("fetching credentials...", "service", svc.Name())
	jr, err := svc.FetchJoinInfo(ctx)
	if err != nil {
		logger.Error("fetch join info failed", "err", err)
		os.Exit(1)
	}
	logger.Info("credentials obtained", "ws_endpoint", jr.WSEndpoint[:50]+"...")

	// connect to signaling
	logger.Info("connecting to signaling...")
	sig, err := svc.ConnectSignaling(ctx, jr, logger.With("component", "signal"))
	if err != nil {
		logger.Error("signaling connect failed", "err", err)
		os.Exit(1)
	}
	defer sig.Close()

	if err := sig.SetKey(cfg.token); err != nil {
		logger.Error("set key failed", "err", err)
		os.Exit(1)
	}

	logger.Info("connected to signaling", "peer_id", sig.PeerID(), "role", cfg.role)
	// No initial sync needed — each test phase self-synchronizes
	// via relay address exchange.

	results := make(map[string][]BenchResult)

	// run raw DTLS test
	logger.Info("=== Starting raw DTLS benchmark ===")
	rawResults, err := benchRawDTLS(ctx, cfg, logger, sig, isSender)
	if err != nil {
		logger.Error("raw DTLS benchmark failed", "err", err)
	} else {
		results["raw-dtls"] = rawResults
	}

	// print results
	if len(results) > 0 {
		printResults(results, cfg.sizeMB, cfg.numConns)
	} else {
		logger.Warn("no results to display")
	}
}

// syncPeers exchanges a sync message with the peer to ensure both are ready.
// After receiving peer's sync, sends extra confirmations so the peer also
// receives at least one message from us.
func syncPeers(ctx context.Context, sig provider.SignalingClient, tag string) error {
	syncTag := "sync:" + tag
	myID := sig.PeerID()

	recvDone := make(chan error, 1)
	go func() {
		for {
			data, err := sig.RecvPayload(ctx, syncTag)
			if err != nil {
				recvDone <- err
				return
			}
			if string(data) != myID {
				recvDone <- nil
				return
			}
			// own echo, keep waiting
		}
	}()

	for {
		_ = sig.SendPayload(ctx, syncTag, []byte(myID))

		select {
		case err := <-recvDone:
			if err != nil {
				return err
			}
			// received peer's sync; send extra messages to ensure peer gets ours
			for i := 0; i < 5; i++ {
				_ = sig.SendPayload(ctx, syncTag, []byte(myID))
				time.Sleep(500 * time.Millisecond)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func printHeader(cfg *config) {
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("  Call-VPN Throughput Benchmark\n")
	fmt.Printf("  Link: %s\n", cfg.callLink)
	fmt.Printf("  Mode: %s | Size: %d MB | Runs: %d | Conns: %d\n",
		cfg.mode, cfg.sizeMB, cfg.runs, cfg.numConns)
	fmt.Println(strings.Repeat("=", 60))
}
