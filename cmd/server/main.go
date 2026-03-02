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
	numConns := flag.Int("n", 4, "number of parallel TURN+DTLS connections (relay mode)")
	useTCP := flag.Bool("tcp", true, "use TCP for TURN connections (relay mode)")
	flag.Parse()

	// Fall back to environment variable if flag not set.
	if *authToken == "" {
		*authToken = os.Getenv("VPN_TOKEN")
	}
	if *callLink == "" {
		*callLink = os.Getenv("VK_CALL_LINK")
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

// runRelayMode starts the server in relay-to-relay mode. Both peers
// join a VK call, exchange relay addresses via WebSocket signaling,
// and establish DTLS connections through TURN relays.
func runRelayMode(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	callLink string, numConns int, useTCP bool, authToken string) {

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

	clientAddrs, _, err := sigClient.RecvRelayAddrs(ctx)
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

	// Match allocations to client addresses (use min of both counts).
	pairCount := len(allocs)
	if len(clientAddrs) < pairCount {
		pairCount = len(clientAddrs)
	}

	// 6. Punch relay for each pair.
	for i := 0; i < pairCount; i++ {
		clientUDP, err := net.ResolveUDPAddr("udp", clientAddrs[i])
		if err != nil {
			logger.Warn("resolve client relay addr", "index", i, "addr", clientAddrs[i], "err", err)
			continue
		}
		if err := internaldtls.PunchRelay(allocs[i].RelayConn, clientUDP); err != nil {
			logger.Warn("punch relay failed", "index", i, "err", err)
		}
	}

	// Short delay for permissions to propagate.
	time.Sleep(500 * time.Millisecond)

	// 7. Accept DTLS connections over TURN relays.
	var cleanups []context.CancelFunc
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	for i := 0; i < pairCount; i++ {
		clientUDP, err := net.ResolveUDPAddr("udp", clientAddrs[i])
		if err != nil {
			continue
		}

		dtlsConn, cleanup, err := internaldtls.AcceptOverTURN(ctx, allocs[i].RelayConn, clientUDP)
		if err != nil {
			logger.Warn("AcceptOverTURN failed", "index", i, "err", err)
			continue
		}
		cleanups = append(cleanups, cleanup)

		logger.Info("relay DTLS connection accepted", "index", i)
		go handleConnection(ctx, logger, siren, dtlsConn, authToken)
	}

	if len(cleanups) == 0 {
		logger.Error("no relay DTLS connections established")
		os.Exit(1)
	}

	logger.Info("relay-to-relay mode active", "connections", len(cleanups))

	// Block until shutdown.
	<-ctx.Done()
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

	sess := &session{
		m:      m,
		logger: sessLogger,
		cancel: sessCancel,
	}
	sessions[id] = sess
	sessionsMu.Unlock()

	// Start AcceptStream loop.
	go func() {
		defer func() {
			sessionsMu.Lock()
			delete(sessions, id)
			sessionsMu.Unlock()
			m.Close()
			sessCancel()
			sessLogger.Info("session closed")
		}()

		for {
			stream, err := m.AcceptStream(sessCtx)
			if err != nil {
				if sessCtx.Err() != nil {
					return
				}
				sessLogger.Warn("accept stream error", "err", err)
				siren.AlertDisconnect(sessCtx, fmt.Sprintf("session-%x", id))
				return
			}
			go handleStream(sessCtx, sessLogger, stream)
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

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(outConn, stream)
	}()

	go func() {
		defer wg.Done()
		io.Copy(stream, outConn)
	}()

	wg.Wait()
}
