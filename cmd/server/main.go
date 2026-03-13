package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/call-vpn/call-vpn/internal/hrtimer"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/server"
)

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:9000", "DTLS UDP listen address")
	authToken := flag.String("token", "", "client auth token (env: VPN_TOKEN, empty = no auth)")
	callLink := flag.String("link", "", "call link ID for relay-to-relay mode (env: VK_CALL_LINK)")
	useTCP := flag.Bool("tcp", true, "use TCP for TURN connections (relay mode)")
	numConns := flag.Int("n", 1, "number of parallel connections (Telemost mode)")
	flag.Parse()

	if *numConns > 8 {
		fmt.Fprintf(os.Stderr, "WARNING: --n=%d exceeds recommended maximum of 8. "+
			"High connection counts may cause VK call instability and potential call blocking.\n", *numConns)
	}

	if *authToken == "" {
		*authToken = os.Getenv("VPN_TOKEN")
	}
	if *callLink == "" {
		*callLink = os.Getenv("VK_CALL_LINK")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := server.Config{
		ListenAddr: *listenAddr,
		AuthToken:  *authToken,
		UseTCP:     *useTCP,
		NumConns:   *numConns,
		Logger:     logger,
	}

	// Enable relay-to-relay mode with the appropriate service.
	if *callLink != "" {
		if telemost.IsTelemostLink(*callLink) {
			cfg.Service = telemost.NewService(*callLink, *authToken)
		} else {
			cfg.Service = vk.NewService(*callLink)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := server.New(cfg)
	srv.Start(ctx)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigCh:
		logger.Info("shutting down server")
		srv.Stop()
	case <-srv.Done():
	}
}
