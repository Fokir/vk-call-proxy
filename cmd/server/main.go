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

	"github.com/call-vpn/call-vpn/internal/captcha"
	_ "github.com/call-vpn/call-vpn/internal/hrtimer"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/server"
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
	listenAddr := flag.String("listen", "0.0.0.0:9000", "DTLS UDP listen address")
	authToken := flag.String("token", "", "client auth token (env: VPN_TOKEN, empty = no auth)")
	var callLinks stringSlice
	flag.Var(&callLinks, "link", "call link ID for relay mode (repeatable for multi-call pool, env: VK_CALL_LINK)")
	useTCP := flag.Bool("tcp", true, "use TCP for TURN connections (relay mode)")
	alsoDirect := flag.Bool("direct", false, "also accept direct DTLS connections on --listen addr alongside relay mode (env: ALSO_DIRECT=1)")
	numConns := flag.Int("n", 1, "number of parallel connections per call")
	var vkTokens stringSlice
	flag.Var(&vkTokens, "vk-token", "VK account token (repeatable, 0-16)")
	verbose := flag.Bool("verbose", false, "enable verbose frame-level logging")
	captchaEndpoint := flag.String("captcha-endpoint", "", "captcha-service URL (e.g. http://captcha:8090, env: CAPTCHA_ENDPOINT)")
	flag.Parse()

	if *verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	if *numConns > 8 {
		fmt.Fprintf(os.Stderr, "WARNING: --n=%d exceeds recommended maximum of 8. "+
			"High connection counts may cause VK call instability and potential call blocking.\n", *numConns)
	}

	if *authToken == "" {
		*authToken = os.Getenv("VPN_TOKEN")
	}
	if len(callLinks) == 0 {
		if env := os.Getenv("VK_CALL_LINK"); env != "" {
			for _, link := range strings.Split(env, ",") {
				link = strings.TrimSpace(link)
				if link != "" {
					callLinks = append(callLinks, link)
				}
			}
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
	if *captchaEndpoint == "" {
		*captchaEndpoint = os.Getenv("CAPTCHA_ENDPOINT")
	}
	if !*alsoDirect {
		if v := os.Getenv("ALSO_DIRECT"); v == "1" || v == "true" {
			*alsoDirect = true
		}
	}

	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	cfg := server.Config{
		ListenAddr: *listenAddr,
		AuthToken:  *authToken,
		VKTokens:   []string(vkTokens),
		AlsoDirect: *alsoDirect,
		UseTCP:     *useTCP,
		NumConns:   *numConns,
		Logger:     logger,
	}

	// Enable relay-to-relay mode with the appropriate service(s).
	if len(callLinks) > 0 {
		var vkOpts []vk.Option
		if *captchaEndpoint != "" {
			vkOpts = append(vkOpts, vk.WithCaptchaSolver(captcha.NewRemoteSolver(*captchaEndpoint)))
		}
		services := make([]provider.Service, len(callLinks))
		for i, link := range callLinks {
			if telemost.IsTelemostLink(link) {
				services[i] = telemost.NewService(link, *authToken)
			} else {
				services[i] = vk.NewService(link, vkOpts...)
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
