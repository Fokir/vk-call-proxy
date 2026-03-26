package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	_ "github.com/call-vpn/call-vpn/internal/hrtimer"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/server"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:9000", "DTLS UDP listen address")
	authToken := flag.String("token", "", "client auth token (env: VPN_TOKEN, empty = no auth)")
	var callLinks stringSlice
	flag.Var(&callLinks, "link", "call link ID for relay mode (repeatable for multi-call pool, env: VK_CALL_LINK)")
	useTCP := flag.Bool("tcp", true, "use TCP for TURN connections (relay mode)")
	numConns := flag.Int("n", 1, "number of parallel connections per call")
	var vkTokens stringSlice
	flag.Var(&vkTokens, "vk-token", "VK account token (repeatable, 0-16)")
	flag.Parse()

	if *numConns > 8 {
		fmt.Fprintf(os.Stderr, "WARNING: --n=%d exceeds recommended maximum of 8. "+
			"High connection counts may cause VK call instability and potential call blocking.\n", *numConns)
	}

	if *authToken == "" {
		*authToken = os.Getenv("VPN_TOKEN")
	}
	if len(callLinks) == 0 {
		if env := os.Getenv("VK_CALL_LINK"); env != "" {
			callLinks = []string{env}
		}
	}
	if len(vkTokens) == 0 {
		if env := os.Getenv("VK_TOKENS"); env != "" {
			vkTokens = strings.Split(env, ",")
		}
	}
	if len(vkTokens) > 16 {
		log.Fatal("maximum 16 VK tokens allowed")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := server.Config{
		ListenAddr: *listenAddr,
		AuthToken:  *authToken,
		VKTokens:   []string(vkTokens),
		UseTCP:     *useTCP,
		NumConns:   *numConns,
		Logger:     logger,
	}

	// Enable relay-to-relay mode with the appropriate service(s).
	if len(callLinks) > 0 {
		services := make([]provider.Service, len(callLinks))
		for i, link := range callLinks {
			if telemost.IsTelemostLink(link) {
				services[i] = telemost.NewService(link, *authToken)
			} else {
				services[i] = vk.NewService(link)
			}
		}
		cfg.Service = services[0]
		if len(services) > 1 {
			cfg.Services = services
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
