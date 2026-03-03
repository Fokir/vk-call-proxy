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
	"sync"
	"syscall"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/netstack"
	internalsignal "github.com/call-vpn/call-vpn/internal/signal"
	"github.com/call-vpn/call-vpn/internal/turn"
)

// session groups multiple DTLS connections from a single client.
type session struct {
	mu     sync.Mutex
	m      *mux.Mux
	logger *slog.Logger
	cancel context.CancelFunc
	conns  int
}

var (
	sessionsMu sync.Mutex
	sessions   = make(map[[16]byte]*session)
)

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:9000", "DTLS UDP listen address")
	authToken := flag.String("token", "", "client auth token (env: VPN_TOKEN, empty = no auth)")
	callLink := flag.String("link", "", "VK call link ID for relay-to-relay mode")
	numConns := flag.Int("n", 16, "number of parallel TURN+DTLS connections (relay mode)")
	useTCP := flag.Bool("tcp", true, "use TCP for TURN connections (relay mode)")
	flag.Parse()

	// Fall back to environment variables if flags not set.
	if *authToken == "" {
		*authToken = os.Getenv("VPN_TOKEN")
	}
	if *callLink == "" {
		*callLink = os.Getenv("VK_CALL_LINK")
	}
	if v := os.Getenv("TURN_CONNS"); v != "" {
		if n, err := fmt.Sscan(v, numConns); n == 1 && err == nil {
			// parsed from env
		}
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	siren := monitoring.New(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutting down server")
		cancel()
	}()

	if *callLink != "" {
		runRelayMode(ctx, logger, siren, *callLink, *numConns, *useTCP, *authToken)
	} else {
		runDirectMode(ctx, logger, siren, *listenAddr, *authToken)
	}
}

// runDirectMode starts the server in direct mode (DTLS/UDP listener).
func runDirectMode(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren, listenAddr, authToken string) {
	ln, err := internaldtls.Listen(listenAddr)
	if err != nil {
		logger.Error("failed to start DTLS listener", "err", err)
		os.Exit(1)
	}
	defer ln.Close()

	logger.Info("server listening (DTLS/UDP, direct mode)", "addr", listenAddr)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("accept error", "err", err)
			continue
		}
		go handleConnection(ctx, logger, siren, conn, authToken)
	}
}

// runRelayMode starts the server in relay-to-relay mode, looping to
// accept successive client sessions. Each iteration creates a fresh
// VK session with new signaling and TURN allocations.
func runRelayMode(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	callLink string, numConns int, useTCP bool, authToken string) {

	logger.Info("starting relay-to-relay mode", "link", callLink, "conns", numConns)

	for {
		if ctx.Err() != nil {
			return
		}

		logger.Info("waiting for client session...")
		err := runOneRelaySession(ctx, logger, siren, callLink, numConns, useTCP, authToken)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.Warn("relay session failed", "err", err)
		} else {
			logger.Info("relay session ended")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// runOneRelaySession handles a single relay-to-relay client session.
// It creates fresh VK credentials, signaling, TURN allocations, and
// a local MUX. Returns when the session ends (all connections die)
// or an error occurs.
func runOneRelaySession(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	callLink string, numConns int, useTCP bool, authToken string) error {

	// 1. Join VK conference to get fresh TURN creds and WS endpoint.
	jr, err := turn.FetchJoinResponse(ctx, callLink)
	if err != nil {
		return fmt.Errorf("join VK conference: %w", err)
	}
	logger.Info("joined VK conference", "ws_endpoint", jr.WSEndpoint, "conv_id", jr.ConvID)

	// 2. Connect to VK WebSocket signaling.
	sigClient, err := internalsignal.Connect(ctx, jr.WSEndpoint, logger.With("component", "signaling"))
	if err != nil {
		return fmt.Errorf("signaling connect: %w", err)
	}
	defer sigClient.Close()

	if err := sigClient.SetKey(authToken); err != nil {
		return fmt.Errorf("set signaling key: %w", err)
	}

	// 3. Create TURN allocations.
	mgr := turn.NewManager(callLink, useTCP, logger)
	defer mgr.CloseAll()

	allocs, err := mgr.Allocate(ctx, numConns)
	if err != nil {
		siren.AlertTURNAuthFailure(ctx, err)
		return fmt.Errorf("allocate TURN connections: %w", err)
	}
	logger.Info("TURN allocations created", "count", len(allocs))

	// Collect our relay addresses.
	ourAddrs := make([]string, len(allocs))
	for i, a := range allocs {
		ourAddrs[i] = a.RelayAddr.String()
	}

	// 4. Exchange relay addresses with retry.
	sendDone := make(chan struct{})
	sendCtx, sendCancel := context.WithCancel(ctx)
	go func() {
		defer close(sendDone)
		for {
			if err := sigClient.SendRelayAddrs(sendCtx, ourAddrs, "server"); err != nil {
				return
			}
			select {
			case <-sendCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()

	clientAddrs, _, err := sigClient.RecvRelayAddrs(ctx, "server")
	if err != nil {
		sendCancel()
		<-sendDone
		return fmt.Errorf("recv relay addrs: %w", err)
	}

	// Keep sending our addrs for a few more seconds so the peer receives them.
	go func() {
		time.Sleep(5 * time.Second)
		sendCancel()
		<-sendDone
	}()

	// Match allocations to client addresses (use min of both counts).
	pairCount := len(allocs)
	if len(clientAddrs) < pairCount {
		pairCount = len(clientAddrs)
	}

	// 5. Punch relay and accept DTLS connections in parallel.
	type dtlsResult struct {
		index   int
		conn    net.Conn
		cleanup context.CancelFunc
		err     error
	}
	results := make(chan dtlsResult, pairCount)
	punchCtx, punchCancel := context.WithCancel(ctx)

	for i := 0; i < pairCount; i++ {
		clientUDP, err := net.ResolveUDPAddr("udp", clientAddrs[i])
		if err != nil {
			logger.Warn("resolve client relay addr", "index", i, "addr", clientAddrs[i], "err", err)
			results <- dtlsResult{index: i, err: err}
			continue
		}
		go func(idx int, relayConn net.PacketConn, addr *net.UDPAddr) {
			// Initial punch + continuous punching during handshake.
			internaldtls.PunchRelay(relayConn, addr)
			go internaldtls.StartPunchLoop(punchCtx, relayConn, addr)
			time.Sleep(500 * time.Millisecond)

			dtlsConn, cleanup, err := internaldtls.AcceptOverTURN(ctx, relayConn, addr)
			results <- dtlsResult{index: idx, conn: dtlsConn, cleanup: cleanup, err: err}
		}(i, allocs[i].RelayConn, clientUDP)
	}

	var dtlsConns []net.Conn
	var cleanups []context.CancelFunc
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	for j := 0; j < pairCount; j++ {
		r := <-results
		if r.err != nil {
			logger.Warn("AcceptOverTURN failed", "index", r.index, "err", r.err)
			continue
		}
		cleanups = append(cleanups, r.cleanup)
		dtlsConns = append(dtlsConns, r.conn)
		logger.Info("relay DTLS connection accepted", "index", r.index)
	}
	punchCancel()

	if len(dtlsConns) == 0 {
		return fmt.Errorf("no relay DTLS connections established")
	}

	logger.Info("relay-to-relay mode active", "connections", len(dtlsConns))

	// 7. Process auth and session protocol on each connection,
	// then create a local MUX for this relay session.
	m := mux.New(logger)
	defer m.Close()

	added := 0
	for i, conn := range dtlsConns {
		if authToken != "" {
			if err := mux.ValidateAuthToken(conn, authToken); err != nil {
				logger.Warn("auth failed on relay conn", "index", i, "err", err)
				conn.Close()
				continue
			}
		}
		sessionID, err := mux.ReadSessionID(conn)
		if err != nil {
			logger.Warn("read session id failed on relay conn", "index", i, "err", err)
			conn.Close()
			continue
		}
		logger.Info("connection received",
			"index", i,
			"session_id", fmt.Sprintf("%x", sessionID),
		)
		m.AddConn(conn)
		added++
	}

	if added == 0 {
		return fmt.Errorf("no connections passed auth/session handshake")
	}

	// 8. Serve streams and raw IP packets (hybrid mode).
	// Idle timeout detects client disconnect: if no frames arrive
	// for 90s (client pings every 30s), readLoop exits → Dead() fires.
	m.EnableRawPackets(256)
	m.EnableStreamAccept(64)
	m.SetIdleTimeout(90 * time.Second)

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	go m.DispatchLoop(sessCtx)
	go m.StartPingLoop(sessCtx, 30*time.Second)

	// Start netstack for raw IP packets (mobile clients).
	ns := netstack.New(logger, m)
	if ns != nil {
		ns.Start(sessCtx)
		defer ns.Close()
	}

	// Drain signaling notifications. On hungup or WS close, reduce idle
	// timeout so connections die quickly if the client is truly gone.
	// We do NOT cancel immediately because VK sends "hungup" for old
	// participants during client reconnection — cancelling would kill
	// active connections prematurely.
	go func() {
		sigClient.WaitForHungup(sessCtx)
		logger.Info("signaling hungup/closed, reducing idle timeout")
		m.SetIdleTimeout(15 * time.Second)
	}()

	// Cancel session context when all MUX connections are dead.
	go func() {
		select {
		case <-m.Dead():
			sessCancel()
		case <-sessCtx.Done():
		}
	}()

	// Accept streams from desktop clients (FrameOpen).
	for {
		select {
		case stream, ok := <-m.AcceptedStreams():
			if !ok {
				return nil
			}
			go handleStream(sessCtx, logger, stream)
		case <-m.Dead():
			return nil
		case <-sessCtx.Done():
			return nil
		}
	}
}

func handleConnection(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren, conn net.Conn, authToken string) {
	// Validate auth token if configured.
	if authToken != "" {
		if err := mux.ValidateAuthToken(conn, authToken); err != nil {
			logger.Warn("auth failed", "remote", conn.RemoteAddr(), "err", err)
			conn.Close()
			return
		}
	}

	// Read session ID (first 16 bytes after DTLS handshake).
	sessionID, err := mux.ReadSessionID(conn)
	if err != nil {
		logger.Warn("read session id failed", "remote", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}

	logger.Info("connection received",
		"remote", conn.RemoteAddr(),
		"session_id", fmt.Sprintf("%x", sessionID),
	)

	sess := getOrCreateSession(ctx, logger, siren, sessionID)

	sess.mu.Lock()
	sess.m.AddConn(conn)
	sess.conns++
	count := sess.conns
	sess.mu.Unlock()

	logger.Info("connection added to session",
		"session_id", fmt.Sprintf("%x", sessionID),
		"total_conns", count,
	)
}

func getOrCreateSession(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren, id [16]byte) *session {
	sessionsMu.Lock()
	if sess, ok := sessions[id]; ok {
		sessionsMu.Unlock()
		return sess
	}

	// First connection for this session — create it while holding the lock.
	sessCtx, sessCancel := context.WithCancel(ctx)
	sessLogger := logger.With("session_id", fmt.Sprintf("%x", id))
	m := mux.New(sessLogger) // Zero initial connections; AddConn later.

	// Enable hybrid mode: both raw IP packets and streams.
	m.EnableRawPackets(256)
	m.EnableStreamAccept(64)
	m.SetIdleTimeout(90 * time.Second)

	sess := &session{
		m:      m,
		logger: sessLogger,
		cancel: sessCancel,
	}
	sessions[id] = sess
	sessionsMu.Unlock()

	// Start DispatchLoop + ping loop.
	go m.DispatchLoop(sessCtx)
	go m.StartPingLoop(sessCtx, 30*time.Second)

	// Start netstack for raw IP packets (mobile clients).
	ns := netstack.New(sessLogger, m)
	if ns != nil {
		ns.Start(sessCtx)
	}

	// Accept streams and handle session lifecycle.
	go func() {
		defer func() {
			sessionsMu.Lock()
			delete(sessions, id)
			sessionsMu.Unlock()
			if ns != nil {
				ns.Close()
			}
			m.Close()
			sessCancel()
			sessLogger.Info("session closed")
		}()

		for {
			select {
			case stream, ok := <-m.AcceptedStreams():
				if !ok {
					return
				}
				go handleStream(sessCtx, sessLogger, stream)
			case <-m.Dead():
				siren.AlertDisconnect(sessCtx, fmt.Sprintf("session-%x", id))
				return
			case <-sessCtx.Done():
				return
			}
		}
	}()

	// Cleanup timer: if session gets no activity, close after timeout.
	go func() {
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		select {
		case <-timer.C:
			// Session timeout — only if no streams were handled
		case <-sessCtx.Done():
		}
	}()

	return sess
}

func handleStream(ctx context.Context, logger *slog.Logger, stream *mux.Stream) {
	defer stream.Close()

	addrBuf := make([]byte, 512)
	n, err := stream.Read(addrBuf)
	if err != nil {
		logger.Debug("read target address failed", "err", err)
		return
	}
	target := string(addrBuf[:n])

	logger.Debug("connecting to target", "stream_id", stream.ID, "target", target)

	dialer := net.Dialer{}
	outConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		logger.Warn("dial target failed", "target", target, "err", err)
		return
	}
	defer outConn.Close()

	buf := make([]byte, mux.MaxFramePayload)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.CopyBuffer(outConn, stream, buf)
	}()

	go func() {
		defer wg.Done()
		buf2 := make([]byte, mux.MaxFramePayload)
		io.CopyBuffer(stream, outConn, buf2)
	}()

	wg.Wait()
}
