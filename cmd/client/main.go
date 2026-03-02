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

	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
	httpproxy "github.com/call-vpn/call-vpn/internal/proxy/http"
	"github.com/call-vpn/call-vpn/internal/proxy/socks5"
	"github.com/call-vpn/call-vpn/internal/turn"
)

func main() {
	socks5Port := flag.Int("socks5-port", 1080, "SOCKS5 proxy listen port")
	httpPort := flag.Int("http-port", 8080, "HTTP/HTTPS proxy listen port")
	callLink := flag.String("link", "", "VK call link ID (e.g. abcd1234)")
	serverAddr := flag.String("server", "", "Remote VPN server address (host:port)")
	numConns := flag.Int("n", 4, "Number of parallel TURN connections")
	useTCP := flag.Bool("tcp", true, "Use TCP for TURN connections (default true)")
	flag.Parse()

	if *callLink == "" || *serverAddr == "" {
		fmt.Fprintln(os.Stderr, "Usage: client --link=<vk-call-link> --server=<host:port> [options]")
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

	// 1. Establish TURN allocations
	logger.Info("establishing TURN connections", "count", *numConns, "link", *callLink)
	mgr := turn.NewManager(*callLink, *useTCP, logger)
	defer mgr.CloseAll()

	allocs, err := mgr.Allocate(ctx, *numConns)
	if err != nil {
		siren.AlertTURNAuthFailure(ctx, err)
		logger.Error("failed to allocate TURN connections", "err", err)
		os.Exit(1)
	}

	logger.Info("TURN connections established", "count", len(allocs))

	// 2. Wrap TURN relay connections for mux
	serverUDPAddr, err := net.ResolveUDPAddr("udp", *serverAddr)
	if err != nil {
		logger.Error("invalid server address", "addr", *serverAddr, "err", err)
		os.Exit(1)
	}

	muxConns := make([]io.ReadWriteCloser, len(allocs))
	for i, a := range allocs {
		muxConns[i] = mux.NewConn(a.RelayConn, serverUDPAddr)
	}

	// 3. Create multiplexer
	m := mux.New(logger, muxConns...)
	defer m.Close()

	// Start dispatch loop for incoming frames
	go m.DispatchLoop(ctx)

	// 4. Stream ID generator
	var nextStreamID atomic.Uint32

	// dialTunnel creates a mux stream that first sends the target address, then relays data.
	dialTunnel := func(ctx context.Context, network, addr string) (io.ReadWriteCloser, error) {
		id := nextStreamID.Add(1)
		stream, err := m.OpenStream(id)
		if err != nil {
			return nil, fmt.Errorf("open stream: %w", err)
		}
		// Send target address as the first message
		if _, err := stream.Write([]byte(addr)); err != nil {
			stream.Close()
			return nil, fmt.Errorf("send target: %w", err)
		}
		return stream, nil
	}

	// 5. Start SOCKS5 proxy
	socks5Addr := fmt.Sprintf("127.0.0.1:%d", *socks5Port)
	socks5Srv := &socks5.Server{
		Addr:   socks5Addr,
		Dial:   dialTunnel,
		Logger: logger.With("proxy", "socks5"),
	}

	// 6. Start HTTP proxy
	httpAddr := fmt.Sprintf("127.0.0.1:%d", *httpPort)
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

	logger.Info("proxies started",
		"socks5", socks5Addr,
		"http", httpAddr,
	)

	// Monitor tunnel health
	go func() {
		active := len(allocs)
		total := *numConns
		if active < total {
			siren.AlertTunnelDegradation(ctx, active, total)
		}
	}()

	// Wait for context cancellation or proxy error
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
