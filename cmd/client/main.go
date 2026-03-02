package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
	httpproxy "github.com/call-vpn/call-vpn/internal/proxy/http"
	"github.com/call-vpn/call-vpn/internal/proxy/socks5"
	sig "github.com/call-vpn/call-vpn/internal/signal"
	"github.com/call-vpn/call-vpn/internal/turn"
)

func main() {
	socks5Port := flag.Int("socks5-port", 1080, "SOCKS5 proxy listen port")
	httpPort := flag.Int("http-port", 8080, "HTTP/HTTPS proxy listen port")
	callLink := flag.String("link", "", "VK call link ID (e.g. abcd1234)")
	numConns := flag.Int("n", 4, "Number of parallel TURN connections")
	useTCP := flag.Bool("tcp", true, "Use TCP for TURN connections")

	// Relay mode flags (default)
	signalURL := flag.String("signal", "", "Signal server URL (e.g. http://server:9443/signal)")
	psk := flag.String("psk", "", "Pre-shared key for authentication")

	// Bind address for proxy listeners
	bindAddr := flag.String("bind", "127.0.0.1", "Bind address for SOCKS5/HTTP proxy listeners")

	// Direct mode flags (legacy)
	directMode := flag.Bool("direct", false, "Use direct TCP mode (legacy)")
	serverAddr := flag.String("server", "", "Direct mode: remote VPN server address (host:port)")

	flag.Parse()

	if *callLink == "" {
		fmt.Fprintln(os.Stderr, "Error: --link is required")
		fmt.Fprintln(os.Stderr, "Usage: client --link=<vk-call-link> --signal=<url> --psk=<secret> [options]")
		fmt.Fprintln(os.Stderr, "       client --link=<vk-call-link> --direct --server=<host:port> [options]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	siren := monitoring.New(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutting down client")
		cancel()
	}()

	if *directMode {
		if *serverAddr == "" {
			fmt.Fprintln(os.Stderr, "Error: --server is required in direct mode")
			os.Exit(1)
		}
		runDirectMode(ctx, logger, siren, *callLink, *serverAddr, *numConns, *useTCP, *socks5Port, *httpPort, *bindAddr)
	} else {
		if *signalURL == "" || *psk == "" {
			fmt.Fprintln(os.Stderr, "Error: --signal and --psk are required in relay mode")
			os.Exit(1)
		}
		runRelayMode(ctx, logger, siren, *callLink, *signalURL, *psk, *numConns, *useTCP, *socks5Port, *httpPort, *bindAddr)
	}
}

// runRelayMode establishes a relay-to-relay VPN session via signaling server.
func runRelayMode(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	callLink, signalURL, psk string, numConns int, useTCP bool, socks5Port, httpPort int, bindAddr string) {

	// 1. Create client-side TURN allocations
	logger.Info("establishing TURN connections", "count", numConns, "link", callLink)
	mgr := turn.NewManager(callLink, useTCP, logger)
	defer mgr.CloseAll()

	allocs, err := mgr.Allocate(ctx, numConns)
	if err != nil {
		siren.AlertTURNAuthFailure(ctx, err)
		logger.Error("failed to allocate TURN connections", "err", err)
		os.Exit(1)
	}
	logger.Info("client TURN connections established", "count", len(allocs))

	// 2. Collect client relay addresses
	clientRelayAddrs := make([]string, len(allocs))
	for i, a := range allocs {
		clientRelayAddrs[i] = a.RelayAddr.String()
	}

	// 3. Signal server to create its TURN allocations
	logger.Info("sending signal request", "url", signalURL)
	resp, err := sig.Connect(ctx, signalURL, sig.ConnectRequest{
		CallLink:         callLink,
		ClientRelayAddrs: clientRelayAddrs,
		PSK:              psk,
		UseTCP:           useTCP,
	})
	if err != nil {
		logger.Error("signal request failed", "err", err)
		os.Exit(1)
	}
	logger.Info("signal response received",
		"session_id", resp.SessionID,
		"server_relays", len(resp.ServerRelayAddrs),
	)

	// 4. Parse server relay addresses and create DatagramConn pairs
	n := len(resp.ServerRelayAddrs)
	if n > len(allocs) {
		n = len(allocs)
	}

	muxConns := make([]io.ReadWriteCloser, n)
	for i := 0; i < n; i++ {
		serverRelayAddr, err := net.ResolveUDPAddr("udp", resp.ServerRelayAddrs[i])
		if err != nil {
			logger.Error("invalid server relay addr", "index", i, "addr", resp.ServerRelayAddrs[i], "err", err)
			os.Exit(1)
		}
		dc := mux.NewDatagramConn(allocs[i].RelayConn, serverRelayAddr)
		// Send permission probe
		if err := dc.SendProbe(); err != nil {
			logger.Warn("permission probe failed", "index", i, "err", err)
		}
		muxConns[i] = dc
	}

	// 5. Wait briefly for permission probes to propagate
	time.Sleep(500 * time.Millisecond)

	// 6. Create multiplexer
	m := mux.New(logger, muxConns...)
	defer m.Close()

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 30*time.Second)

	// 7. Start proxies
	startProxies(ctx, logger, siren, m, len(allocs), numConns, socks5Port, httpPort, bindAddr)
}

// runDirectMode uses the legacy direct TCP connection to the VPN server.
func runDirectMode(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	callLink, serverAddr string, numConns int, useTCP bool, socks5Port, httpPort int, bindAddr string) {

	logger.Info("establishing TURN connections", "count", numConns, "link", callLink)
	mgr := turn.NewManager(callLink, useTCP, logger)
	defer mgr.CloseAll()

	allocs, err := mgr.Allocate(ctx, numConns)
	if err != nil {
		siren.AlertTURNAuthFailure(ctx, err)
		logger.Error("failed to allocate TURN connections", "err", err)
		os.Exit(1)
	}
	logger.Info("TURN connections established", "count", len(allocs))

	serverUDPAddr, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		logger.Error("invalid server address", "addr", serverAddr, "err", err)
		os.Exit(1)
	}

	muxConns := make([]io.ReadWriteCloser, len(allocs))
	for i, a := range allocs {
		muxConns[i] = mux.NewConn(a.RelayConn, serverUDPAddr)
	}

	m := mux.New(logger, muxConns...)
	defer m.Close()

	go m.DispatchLoop(ctx)

	startProxies(ctx, logger, siren, m, len(allocs), numConns, socks5Port, httpPort, bindAddr)
}

// startProxies starts SOCKS5 and HTTP proxies over the given Mux.
func startProxies(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	m *mux.Mux, activeConns, totalConns, socks5Port, httpPort int, bindAddr string) {

	var nextStreamID atomic.Uint32

	dialTunnel := func(ctx context.Context, network, addr string) (io.ReadWriteCloser, error) {
		id := nextStreamID.Add(1)
		stream, err := m.OpenStream(id)
		if err != nil {
			return nil, fmt.Errorf("open stream: %w", err)
		}
		if _, err := stream.Write([]byte(addr)); err != nil {
			stream.Close()
			return nil, fmt.Errorf("send target: %w", err)
		}
		return stream, nil
	}

	socks5Addr := fmt.Sprintf("%s:%d", bindAddr, socks5Port)
	socks5Srv := &socks5.Server{
		Addr:   socks5Addr,
		Dial:   dialTunnel,
		Logger: logger.With("proxy", "socks5"),
	}

	httpAddr := fmt.Sprintf("%s:%d", bindAddr, httpPort)
	httpSrv := &httpproxy.Server{
		Addr:   httpAddr,
		Dial:   dialTunnel,
		Logger: logger.With("proxy", "http"),
	}

	errCh := make(chan error, 2)

	go func() {
		errCh <- socks5Srv.ListenAndServe(ctx)
	}()

	go func() {
		errCh <- httpSrv.ListenAndServe(ctx)
	}()

	logger.Info("proxies started", "socks5", socks5Addr, "http", httpAddr)

	go func() {
		if activeConns < totalConns {
			siren.AlertTunnelDegradation(ctx, activeConns, totalConns)
		}
	}()

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("proxy error", "err", err)
		}
	case <-ctx.Done():
	}

	socks5Srv.Close()
	httpSrv.Close()
	logger.Info("client stopped")
}
