package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/call-vpn/call-vpn/internal/bypass"
	"github.com/call-vpn/call-vpn/internal/captcha"
	"github.com/call-vpn/call-vpn/internal/client"
	_ "github.com/call-vpn/call-vpn/internal/hrtimer"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/scripts"
	"github.com/call-vpn/call-vpn/internal/scriptshook"
	"github.com/call-vpn/call-vpn/internal/tunnel"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

func main() {
	socks5Port := flag.Int("socks5-port", 1080, "SOCKS5 proxy listen port")
	httpPort := flag.Int("http-port", 8080, "HTTP/HTTPS proxy listen port")
	var callLinks stringSlice
	flag.Var(&callLinks, "link", "call link ID (repeatable for multi-call pool)")
	numConns := flag.Int("n", 16, "Number of parallel TURN+DTLS connections")
	useTCP := flag.Bool("tcp", true, "Use TCP for TURN connections")
	serverAddr := flag.String("server", "", "VPN server address (host:port), empty = relay-to-relay mode")
	bindAddr := flag.String("bind", "127.0.0.1", "Bind address for SOCKS5/HTTP proxy listeners")
	authToken := flag.String("token", "", "auth token for server")
	noBypass := flag.Bool("no-bypass", false, "disable built-in bypass for Russian services (VK, Yandex, Gosuslugi, etc.)")
	fingerprint := flag.String("fingerprint", "", "server DTLS certificate SHA-256 fingerprint (hex)")
	var vkTokens stringSlice
	flag.Var(&vkTokens, "vk-token", "VK account token (repeatable, 0-16)")
	verbose := flag.Bool("verbose", false, "enable verbose frame-level logging")
	captchaEndpoint := flag.String("captcha-endpoint", "", "captcha-service URL, empty = open system browser")
	scriptsFlags := scripts.RegisterFlags(flag.CommandLine)

	flag.Parse()

	if *verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	if len(vkTokens) == 0 {
		if env := os.Getenv("VK_TOKENS"); env != "" {
			vkTokens = strings.Split(env, ",")
		}
	}
	if len(vkTokens) > 16 {
		log.Fatal("maximum 16 VK tokens allowed")
	}

	var fpBytes []byte
	if *fingerprint != "" {
		var err error
		fpBytes, err = hex.DecodeString(*fingerprint)
		if err != nil || len(fpBytes) != 32 {
			log.Fatalf("invalid fingerprint: must be 64 hex characters (SHA-256)")
		}
	}

	if *numConns > 8 {
		fmt.Fprintf(os.Stderr, "WARNING: --n=%d exceeds recommended maximum of 8. "+
			"High connection counts may cause VK call instability and potential call blocking.\n", *numConns)
	}

	if len(callLinks) == 0 {
		fmt.Fprintln(os.Stderr, "Error: --link is required")
		fmt.Fprintln(os.Stderr, "Usage: client --link=<call-link> [--server=<host:port>] [options]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	siren := monitoring.New(logger)

	ctxRoot, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	scriptsMgr := scripts.NewManager(scriptsFlags.BuildConfig(scripts.NewSlogLogger(logger.With("component", "scripts"))))
	if err := scriptsMgr.Start(ctxRoot); err != nil {
		logger.Warn("scripts manager start failed", "err", err)
	}
	defer scriptsMgr.Stop()
	scriptshook.Register(scriptsMgr)

	// Create captcha solver.
	var solver provider.CaptchaSolver
	if *captchaEndpoint != "" {
		solver = captcha.NewChainSolver(
			captcha.NewDirectSolver(),
			captcha.NewRemoteSolver(*captchaEndpoint),
		)
	} else {
		solver = captcha.NewChainSolver(
			captcha.NewDirectSolver(),
			captcha.NewInteractiveSolver(),
		)
	}

	// Create call service providers (auto-detect from link).
	services := make([]provider.Service, len(callLinks))
	for i, link := range callLinks {
		if telemost.IsTelemostLink(link) {
			services[i] = telemost.NewService(link, *authToken)
		} else {
			services[i] = vk.NewService(link, vk.WithCaptchaSolver(solver))
		}
	}

	var bypassMatcher *bypass.Matcher
	if !*noBypass {
		bypassMatcher = bypass.New(bypass.DefaultRussianServices())
		logger.Info("bypass enabled for Russian services (VK, Yandex, Gosuslugi, etc.)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutting down client")
		cancel()
	}()

	// Multi-call pool mode: N links → shared MUX for fault tolerance
	if len(callLinks) > 1 {
		logger.Info("multi-call pool mode", "calls", len(callLinks), "conns_per_call", *numConns)
		pool := tunnel.NewCallPool(tunnel.PoolConfig{
			Services:     services,
			ConnsPerCall: *numConns,
			UseTCP:       *useTCP,
			AuthToken:    *authToken,
			VKTokens:     []string(vkTokens),
			Fingerprint:  fpBytes,
			Logger:       logger,
		})
		defer pool.Close()

		client.RunMultiRelay(ctx, &client.MultiRelayConfig{
			Pool:          pool,
			SocksPort:     *socks5Port,
			HTTPPort:      *httpPort,
			BindAddr:      *bindAddr,
			BypassMatcher: bypassMatcher,
			Logger:        logger,
			Siren:         siren,
		})
		return
	}

	// Single-link mode: backward compatible
	svc := services[0]
	cfg := &client.Config{
		Service:       svc,
		ServerAddr:    *serverAddr,
		NumConns:      *numConns,
		UseTCP:        *useTCP,
		AuthToken:     *authToken,
		VKTokens:      []string(vkTokens),
		Fingerprint:   fpBytes,
		SocksPort:     *socks5Port,
		HTTPPort:      *httpPort,
		BindAddr:      *bindAddr,
		BypassMatcher: bypassMatcher,
		Logger:        logger,
		Siren:         siren,
	}

	// Telemost uses WebRTC DataChannel through SFU (no raw TURN).
	if _, ok := svc.(*telemost.Service); ok {
		client.RunTelemost(ctx, cfg)
	} else if *serverAddr != "" {
		client.RunDirect(ctx, cfg)
	} else {
		client.RunRelayToRelay(ctx, cfg)
	}
}
