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

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
	httpproxy "github.com/call-vpn/call-vpn/internal/proxy/http"
	"github.com/call-vpn/call-vpn/internal/proxy/socks5"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

func main() {
	socks5Port := flag.Int("socks5-port", 1080, "SOCKS5 proxy listen port")
	httpPort := flag.Int("http-port", 8080, "HTTP/HTTPS proxy listen port")
	callLink := flag.String("link", "", "VK call link ID (e.g. abcd1234)")
	numConns := flag.Int("n", 4, "Number of parallel TURN+DTLS connections")
	useTCP := flag.Bool("tcp", true, "Use TCP for TURN connections")
	serverAddr := flag.String("server", "", "VPN server address (host:port)")
	bindAddr := flag.String("bind", "127.0.0.1", "Bind address for SOCKS5/HTTP proxy listeners")

	flag.Parse()

	if *callLink == "" || *serverAddr == "" {
		fmt.Fprintln(os.Stderr, "Error: --link and --server are required")
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

	run(ctx, logger, siren, *callLink, *serverAddr, *numConns, *useTCP, *socks5Port, *httpPort, *bindAddr)
}

func run(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	callLink, server string, numConns int, useTCP bool, socks5Port, httpPort int, bindAddr string) {

	serverUDPAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		logger.Error("invalid server address", "addr", server, "err", err)
		os.Exit(1)
	}

	sessionID := uuid.New()
	logger.Info("session", "id", sessionID.String())

	// 1. Create TURN allocations.
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

	// 2. Establish DTLS-over-TURN connections and send session ID.
	var cleanups []context.CancelFunc
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	var muxConns []io.ReadWriteCloser
	for i, alloc := range allocs {
		dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, alloc.RelayConn, serverUDPAddr)
		if err != nil {
			logger.Warn("DTLS-over-TURN failed", "index", i, "err", err)
			continue
		}
		cleanups = append(cleanups, cleanup)

		// Send session UUID so server can group connections.
		var sid [16]byte
		copy(sid[:], sessionID[:])
		if err := mux.WriteSessionID(dtlsConn, sid); err != nil {
			logger.Warn("write session id failed", "index", i, "err", err)
			cleanup()
			continue
		}

		muxConns = append(muxConns, dtlsConn)
		logger.Info("DTLS connection established", "index", i)
	}

	if len(muxConns) == 0 {
		logger.Error("no DTLS connections established")
		os.Exit(1)
	}

	// 3. Create multiplexer over DTLS connections.
	m := mux.New(logger, muxConns...)
	defer m.Close()

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 30*time.Second)

	// 4. Start proxies.
	startProxies(ctx, logger, siren, m, len(muxConns), numConns, socks5Port, httpPort, bindAddr)
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
