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
	internalsignal "github.com/call-vpn/call-vpn/internal/signal"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

func main() {
	socks5Port := flag.Int("socks5-port", 1080, "SOCKS5 proxy listen port")
	httpPort := flag.Int("http-port", 8080, "HTTP/HTTPS proxy listen port")
	callLink := flag.String("link", "", "VK call link ID (e.g. abcd1234)")
	numConns := flag.Int("n", 16, "Number of parallel TURN+DTLS connections")
	useTCP := flag.Bool("tcp", true, "Use TCP for TURN connections")
	serverAddr := flag.String("server", "", "VPN server address (host:port), empty = relay-to-relay mode")
	bindAddr := flag.String("bind", "127.0.0.1", "Bind address for SOCKS5/HTTP proxy listeners")
	authToken := flag.String("token", "", "auth token for server")

	flag.Parse()

	if *callLink == "" {
		fmt.Fprintln(os.Stderr, "Error: --link is required")
		fmt.Fprintln(os.Stderr, "Usage: client --link=<vk-call-link> [--server=<host:port>] [options]")
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

	if *serverAddr != "" {
		runDirect(ctx, logger, siren, *callLink, *serverAddr, *numConns, *useTCP, *socks5Port, *httpPort, *bindAddr, *authToken)
	} else {
		runRelayToRelay(ctx, logger, siren, *callLink, *numConns, *useTCP, *socks5Port, *httpPort, *bindAddr, *authToken)
	}
}

// runDirect connects through TURN to a server listening on a direct UDP address.
func runDirect(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	callLink, server string, numConns int, useTCP bool, socks5Port, httpPort int, bindAddr, authToken string) {

	serverUDPAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		logger.Error("invalid server address", "addr", server, "err", err)
		os.Exit(1)
	}

	sessionID := uuid.New()
	logger.Info("session (direct mode)", "id", sessionID.String())

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

		// Send auth token if configured.
		if authToken != "" {
			if err := mux.WriteAuthToken(dtlsConn, authToken); err != nil {
				logger.Warn("write auth token failed", "index", i, "err", err)
				cleanup()
				continue
			}
		}

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

// runRelayToRelay connects through VK TURN relays to a server that also
// joins the same VK call. Relay addresses are exchanged via VK WebSocket signaling.
func runRelayToRelay(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	callLink string, numConns int, useTCP bool, socks5Port, httpPort int, bindAddr, authToken string) {

	logger.Info("starting relay-to-relay mode", "link", callLink, "conns", numConns)

	// 1. Join VK conference to get TURN creds and WS endpoint.
	jr, err := turn.FetchJoinResponse(ctx, callLink)
	if err != nil {
		logger.Error("failed to join VK conference", "err", err)
		os.Exit(1)
	}
	logger.Info("joined VK conference", "ws_endpoint", jr.WSEndpoint, "conv_id", jr.ConvID)

	// 2. Connect to VK WebSocket signaling.
	sigClient, err := internalsignal.Connect(ctx, jr.WSEndpoint, logger.With("component", "signaling"))
	if err != nil {
		logger.Error("signaling connect failed", "err", err)
		os.Exit(1)
	}
	defer sigClient.Close()

	if err := sigClient.SetKey(authToken); err != nil {
		logger.Error("set signaling key failed", "err", err)
		os.Exit(1)
	}

	// Tell server to kill any existing session so it's ready for us.
	_ = sigClient.SendDisconnect(ctx)

	// 3. Create TURN allocations.
	mgr := turn.NewManager(callLink, useTCP, logger)
	defer mgr.CloseAll()

	allocs, err := mgr.Allocate(ctx, numConns)
	if err != nil {
		siren.AlertTURNAuthFailure(ctx, err)
		logger.Error("failed to allocate TURN connections", "err", err)
		os.Exit(1)
	}
	logger.Info("TURN allocations created", "count", len(allocs))

	// Collect our relay addresses.
	ourAddrs := make([]string, len(allocs))
	for i, a := range allocs {
		ourAddrs[i] = a.RelayAddr.String()
	}

	// 4. Exchange relay addresses with retry.
	// Send our addresses periodically until the peer receives them.
	sendDone := make(chan struct{})
	sendCtx, sendCancel := context.WithCancel(ctx)
	go func() {
		defer close(sendDone)
		for {
			if err := sigClient.SendRelayAddrs(sendCtx, ourAddrs, "client"); err != nil {
				return
			}
			select {
			case <-sendCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()

	serverAddrs, _, err := sigClient.RecvRelayAddrs(ctx, "client")
	if err != nil {
		sendCancel()
		<-sendDone
		logger.Error("recv relay addrs failed", "err", err)
		os.Exit(1)
	}

	// Keep sending our addrs for a few more seconds so the peer receives them.
	go func() {
		time.Sleep(5 * time.Second)
		sendCancel()
		<-sendDone
	}()

	// Match allocations to server addresses.
	pairCount := len(allocs)
	if len(serverAddrs) < pairCount {
		pairCount = len(serverAddrs)
	}

	// 6. Punch relay and establish DTLS in parallel.
	sessionID := uuid.New()
	logger.Info("session (relay-to-relay mode)", "id", sessionID.String())

	type dtlsResult struct {
		index   int
		conn    io.ReadWriteCloser
		cleanup context.CancelFunc
		err     error
	}
	results := make(chan dtlsResult, pairCount)
	punchCtx, punchCancel := context.WithCancel(ctx)

	for i := 0; i < pairCount; i++ {
		serverUDP, err := net.ResolveUDPAddr("udp", serverAddrs[i])
		if err != nil {
			logger.Warn("resolve server relay addr", "index", i, "addr", serverAddrs[i], "err", err)
			results <- dtlsResult{index: i, err: err}
			continue
		}
		go func(idx int, relayConn net.PacketConn, addr *net.UDPAddr) {
			internaldtls.PunchRelay(relayConn, addr)
			go internaldtls.StartPunchLoop(punchCtx, relayConn, addr)
			time.Sleep(500 * time.Millisecond)

			dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, relayConn, addr)
			if err != nil {
				results <- dtlsResult{index: idx, err: err}
				return
			}

			if authToken != "" {
				if err := mux.WriteAuthToken(dtlsConn, authToken); err != nil {
					cleanup()
					results <- dtlsResult{index: idx, err: fmt.Errorf("write auth token: %w", err)}
					return
				}
			}

			var sid [16]byte
			copy(sid[:], sessionID[:])
			if err := mux.WriteSessionID(dtlsConn, sid); err != nil {
				cleanup()
				results <- dtlsResult{index: idx, err: fmt.Errorf("write session id: %w", err)}
				return
			}

			results <- dtlsResult{index: idx, conn: dtlsConn, cleanup: cleanup}
		}(i, allocs[i].RelayConn, serverUDP)
	}

	var cleanups []context.CancelFunc
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	var muxConns []io.ReadWriteCloser
	for j := 0; j < pairCount; j++ {
		r := <-results
		if r.err != nil {
			logger.Warn("relay DTLS failed", "index", r.index, "err", r.err)
			continue
		}
		cleanups = append(cleanups, r.cleanup)
		muxConns = append(muxConns, r.conn)
		logger.Info("relay DTLS connection established", "index", r.index)
	}
	punchCancel()

	if len(muxConns) == 0 {
		logger.Error("no relay DTLS connections established")
		os.Exit(1)
	}

	// 8. Create multiplexer over relay DTLS connections.
	m := mux.New(logger, muxConns...)
	defer m.Close()

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 30*time.Second)

	// 9. Start per-connection reconnect goroutine.
	go reconnectConns(ctx, sigClient, mgr, m, sessionID, authToken, logger)

	// 10. Monitor signaling for session end.
	go func() {
		reason := sigClient.WaitForSessionEnd(ctx)
		if reason == internalsignal.SessionEndHungup {
			logger.Info("signaling hungup (server left call)")
			sigClient.DrainAndRoute(ctx)
		}
	}()

	// Reconnect initially failed connections.
	if missing := numConns - len(muxConns); missing > 0 {
		logger.Info("attempting to reconnect initially failed connections", "missing", missing)
		go func() {
			ackCh, unsub := sigClient.Subscribe(internalsignal.WireConnOk, 8)
			defer unsub()
			for i := 0; i < missing; i++ {
				m.BeginReconnect()
				if err := reconnectOne(ctx, sigClient, mgr, m, ackCh, sessionID, authToken, logger); err != nil {
					logger.Warn("initial reconnect failed", "err", err)
				}
				m.EndReconnect()
			}
		}()
	}

	// 11. Start proxies.
	startProxies(ctx, logger, siren, m, len(muxConns), numConns, socks5Port, httpPort, bindAddr)
}

// reconnectConns monitors ConnDied and reconnects individual dead connections
// via VK signaling. Each dead connection triggers up to 3 reconnect attempts.
func reconnectConns(ctx context.Context, sigClient *internalsignal.Client,
	mgr *turn.Manager, m *mux.Mux, sessionID uuid.UUID, authToken string, logger *slog.Logger) {

	ackCh, unsub := sigClient.Subscribe(internalsignal.WireConnOk, 8)
	defer unsub()

	for {
		select {
		case <-ctx.Done():
			return
		case idx, ok := <-m.ConnDied():
			if !ok {
				return
			}
			logger.Info("connection died, starting reconnect", "index", idx)
			m.RemoveConn(idx)
			m.BeginReconnect()

			go func(deadIdx int) {
				defer m.EndReconnect()
				var err error
				for attempt := 1; attempt <= 3; attempt++ {
					err = reconnectOne(ctx, sigClient, mgr, m, ackCh, sessionID, authToken, logger)
					if err == nil {
						logger.Info("reconnected connection successfully", "dead_index", deadIdx, "attempt", attempt)
						return
					}
					logger.Warn("reconnect attempt failed", "dead_index", deadIdx, "attempt", attempt, "err", err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Duration(attempt*2) * time.Second):
					}
				}
				logger.Error("failed to reconnect after 3 attempts", "dead_index", deadIdx)
			}(idx)
		}
	}
}

// reconnectOne allocates a new TURN relay, sends its address to the server
// via signaling, waits for the server's relay address, then establishes
// a new DTLS connection and adds it to the MUX.
func reconnectOne(ctx context.Context, sigClient *internalsignal.Client,
	mgr *turn.Manager, m *mux.Mux, ackCh <-chan []byte,
	sessionID uuid.UUID, authToken string, logger *slog.Logger) error {

	allocs, err := mgr.Allocate(ctx, 1)
	if err != nil {
		return fmt.Errorf("allocate TURN: %w", err)
	}
	alloc := allocs[0]
	myAddr := alloc.RelayAddr.String()

	// Send our new relay address to server.
	if err := sigClient.SendPayload(ctx, internalsignal.WireConnNew, []byte(myAddr)); err != nil {
		return fmt.Errorf("send conn-new: %w", err)
	}

	// Wait for server's relay address (timeout 15s).
	var serverAddr string
	select {
	case payload, ok := <-ackCh:
		if !ok {
			return fmt.Errorf("ack channel closed")
		}
		serverAddr = string(payload)
	case <-time.After(15 * time.Second):
		return fmt.Errorf("timeout waiting for server relay addr")
	case <-ctx.Done():
		return ctx.Err()
	}

	serverUDP, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return fmt.Errorf("resolve server addr: %w", err)
	}

	// Punch and DTLS dial.
	punchCtx, punchCancel := context.WithCancel(ctx)
	defer punchCancel()
	internaldtls.PunchRelay(alloc.RelayConn, serverUDP)
	go internaldtls.StartPunchLoop(punchCtx, alloc.RelayConn, serverUDP)
	time.Sleep(500 * time.Millisecond)

	dtlsConn, _, err := internaldtls.DialOverTURN(ctx, alloc.RelayConn, serverUDP)
	if err != nil {
		return fmt.Errorf("DialOverTURN: %w", err)
	}
	punchCancel()

	// Auth + session ID.
	if authToken != "" {
		if err := mux.WriteAuthToken(dtlsConn, authToken); err != nil {
			dtlsConn.Close()
			return fmt.Errorf("write auth token: %w", err)
		}
	}
	var sid [16]byte
	copy(sid[:], sessionID[:])
	if err := mux.WriteSessionID(dtlsConn, sid); err != nil {
		dtlsConn.Close()
		return fmt.Errorf("write session id: %w", err)
	}

	m.AddConn(dtlsConn)
	logger.Info("reconnect: new connection added to MUX")
	return nil
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

	// Wait for both proxies or context cancellation.
	// A single proxy bind failure should not kill the client.
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
