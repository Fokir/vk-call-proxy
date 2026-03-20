package client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/call-vpn/call-vpn/internal/bypass"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
	httpproxy "github.com/call-vpn/call-vpn/internal/proxy/http"
	"github.com/call-vpn/call-vpn/internal/proxy/socks5"
)

// startProxies starts SOCKS5 and HTTP proxies over the given Mux.
func startProxies(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	m *mux.Mux, activeConns, totalConns, socks5Port, httpPort int, bindAddr string,
	bypassMatcher *bypass.Matcher) {

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
		Bypass: bypassMatcher,
		Logger: logger.With("proxy", "socks5"),
	}

	httpAddr := fmt.Sprintf("%s:%d", bindAddr, httpPort)
	httpSrv := &httpproxy.Server{
		Addr:   httpAddr,
		Dial:   dialTunnel,
		Bypass: bypassMatcher,
		Logger: logger.With("proxy", "http"),
	}

	errCh := make(chan error, 2)

	go func() {
		errCh <- socks5Srv.ListenAndServe(ctx)
	}()

	go func() {
		errCh <- httpSrv.ListenAndServe(ctx)
	}()

	logger.Info("proxies started", "socks5", socks5Addr, "http", httpAddr,
		"active_conns", m.ActiveConns(), "target_conns", totalConns)

	go func() {
		if activeConns < totalConns {
			siren.AlertTunnelDegradation(ctx, activeConns, totalConns)
		}
	}()

	// Periodically log connection status.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				logger.Info("connection status",
					"active", m.ActiveConns(),
					"total", m.TotalConns(),
					"target", totalConns,
				)
			}
		}
	}()

	// Wait for both proxies or context cancellation.
	remaining := 2
	for remaining > 0 {
		select {
		case err := <-errCh:
			remaining--
			if err != nil {
				logger.Warn("proxy error", "err", err)
			}
		case <-ctx.Done():
			remaining = 0
		}
	}

	socks5Srv.Close()
	httpSrv.Close()
	logger.Info("client stopped")
}

// startRelayProxies is like startProxies but resolves the MUX dynamically
// via getMux, so that full session reconnects transparently switch the MUX
// underneath active proxy listeners.
func startRelayProxies(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	getMux func() *mux.Mux, totalConns, socks5Port, httpPort int, bindAddr string,
	bypassMatcher *bypass.Matcher) {

	var nextStreamID atomic.Uint32

	dialTunnel := func(ctx context.Context, network, addr string) (io.ReadWriteCloser, error) {
		m := getMux()
		if m == nil {
			return nil, fmt.Errorf("tunnel not available")
		}
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
		Bypass: bypassMatcher,
		Logger: logger.With("proxy", "socks5"),
	}

	httpAddr := fmt.Sprintf("%s:%d", bindAddr, httpPort)
	httpSrv := &httpproxy.Server{
		Addr:   httpAddr,
		Dial:   dialTunnel,
		Bypass: bypassMatcher,
		Logger: logger.With("proxy", "http"),
	}

	errCh := make(chan error, 2)

	go func() {
		errCh <- socks5Srv.ListenAndServe(ctx)
	}()

	go func() {
		errCh <- httpSrv.ListenAndServe(ctx)
	}()

	m := getMux()
	activeConns := 0
	if m != nil {
		activeConns = m.ActiveConns()
	}
	logger.Info("proxies started", "socks5", socks5Addr, "http", httpAddr,
		"active_conns", activeConns, "target_conns", totalConns)

	go func() {
		if activeConns < totalConns {
			siren.AlertTunnelDegradation(ctx, activeConns, totalConns)
		}
	}()

	// Periodically log connection status.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m := getMux()
				if m != nil {
					logger.Info("connection status",
						"active", m.ActiveConns(),
						"total", m.TotalConns(),
						"target", totalConns,
					)
				}
			}
		}
	}()

	remaining := 2
	for remaining > 0 {
		select {
		case err := <-errCh:
			remaining--
			if err != nil {
				logger.Warn("proxy error", "err", err)
			}
		case <-ctx.Done():
			remaining = 0
		}
	}

	socks5Srv.Close()
	httpSrv.Close()
	logger.Info("client stopped")
}
