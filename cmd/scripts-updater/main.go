// scripts-updater is a standalone binary that polls the configured scripts
// URL, verifies and writes the bundle into a shared volume. Other services
// (server, captcha-service, server-ui) mount the same volume read-only and
// pick up changes via the manager's store-watch mechanism.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/call-vpn/call-vpn/internal/scripts"
)

func main() {
	scriptsFlags := scripts.RegisterFlags(flag.CommandLine)
	verbose := flag.Bool("verbose", false, "verbose logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg := scriptsFlags.BuildConfig(scripts.NewSlogLogger(logger))
	if cfg.URL == "" {
		fmt.Fprintln(os.Stderr, "scripts-updater: --scripts-url is required (or set CALLVPN_SCRIPTS_URL)")
		os.Exit(2)
	}
	if cfg.PublicKey == "" {
		fmt.Fprintln(os.Stderr, "scripts-updater: --scripts-pubkey is required (or set CALLVPN_SCRIPTS_PUBKEY)")
		os.Exit(2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := scripts.NewManager(cfg)
	if err := mgr.Start(ctx); err != nil {
		logger.Error("start", "err", err)
		os.Exit(1)
	}
	defer mgr.Stop()

	logger.Info("scripts-updater running",
		"url", cfg.URL, "localDir", cfg.LocalDir, "interval", cfg.CheckInterval)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	logger.Info("shutting down")
}
