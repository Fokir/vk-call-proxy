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
	"github.com/call-vpn/call-vpn/internal/client"
	_ "github.com/call-vpn/call-vpn/internal/hrtimer"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	socks5Port := flag.Int("socks5-port", 1080, "SOCKS5 proxy listen port")
	httpPort := flag.Int("http-port", 8080, "HTTP/HTTPS proxy listen port")
	callLink := flag.String("link", "", "call link ID (e.g. abcd1234)")
	numConns := flag.Int("n", 16, "Number of parallel TURN+DTLS connections")
	useTCP := flag.Bool("tcp", true, "Use TCP for TURN connections")
	serverAddr := flag.String("server", "", "VPN server address (host:port), empty = relay-to-relay mode")
	bindAddr := flag.String("bind", "127.0.0.1", "Bind address for SOCKS5/HTTP proxy listeners")
	authToken := flag.String("token", "", "auth token for server")
	noBypass := flag.Bool("no-bypass", false, "disable built-in bypass for Russian services (VK, Yandex, Gosuslugi, etc.)")
	fingerprint := flag.String("fingerprint", "", "server DTLS certificate SHA-256 fingerprint (hex)")
	var vkTokens stringSlice
	flag.Var(&vkTokens, "vk-token", "VK account token (repeatable, 0-16)")

	flag.Parse()

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

	if *callLink == "" {
		fmt.Fprintln(os.Stderr, "Error: --link is required")
		fmt.Fprintln(os.Stderr, "Usage: client --link=<call-link> [--server=<host:port>] [options]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	siren := monitoring.New(logger)

	// Create call service provider (auto-detect from link).
	var svc provider.Service
	if telemost.IsTelemostLink(*callLink) {
		svc = telemost.NewService(*callLink, *authToken)
	} else {
		svc = vk.NewService(*callLink)
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
