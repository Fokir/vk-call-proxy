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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/call-vpn/call-vpn/internal/bypass"
	_ "github.com/call-vpn/call-vpn/internal/hrtimer"
	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	httpproxy "github.com/call-vpn/call-vpn/internal/proxy/http"
	"github.com/call-vpn/call-vpn/internal/proxy/socks5"
	internalsignal "github.com/call-vpn/call-vpn/internal/signal"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

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

	flag.Parse()

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

	// Telemost uses WebRTC DataChannel through SFU (no raw TURN).
	if tmSvc, ok := svc.(*telemost.Service); ok {
		runTelemost(ctx, logger, siren, tmSvc, *numConns, *socks5Port, *httpPort, *bindAddr, *authToken, bypassMatcher)
	} else if *serverAddr != "" {
		runDirect(ctx, logger, siren, svc, *serverAddr, *numConns, *useTCP, *socks5Port, *httpPort, *bindAddr, *authToken, bypassMatcher)
	} else {
		runRelayToRelay(ctx, logger, siren, svc, *numConns, *useTCP, *socks5Port, *httpPort, *bindAddr, *authToken, bypassMatcher)
	}
}

// runTelemost connects through Telemost's Goloom SFU via WebRTC DataChannels.
// Both client and server join the same Telemost meeting; the SFU forwards
// DataChannel data between participants.
func runTelemost(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	svc *telemost.Service, numConns int, socks5Port, httpPort int, bindAddr, authToken string,
	bypassMatcher *bypass.Matcher) {

	logger.Info("starting Telemost WebRTC mode", "service", svc.Name(), "conns", numConns)

	// Derive deterministic display names from auth token for 1:1 pairing.
	serverNames, clientNames := provider.DeriveDisplayNames(authToken, numConns)

	var muxConns []io.ReadWriteCloser
	var cleanups []func()

	for i := 0; i < numConns; i++ {
		myName := clientNames[i]
		peerName := serverNames[i]
		conn, cleanup, err := svc.ConnectPaired(ctx, logger.With("index", i), myName, peerName, i)
		if err != nil {
			logger.Warn("Telemost WebRTC connection failed", "index", i, "err", err)
			continue
		}
		muxConns = append(muxConns, conn)
		cleanups = append(cleanups, cleanup)
		logger.Info("Telemost WebRTC connection established", "index", i, "progress", fmt.Sprintf("%d/%d", len(muxConns), numConns))
	}

	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	if len(muxConns) == 0 {
		logger.Error("no Telemost connections established")
		os.Exit(1)
	}

	// Send auth token + session UUID on each connection before MUX takes over.
	sessionID := uuid.New()
	var sid [16]byte
	copy(sid[:], sessionID[:])
	logger.Info("session (Telemost)", "id", sessionID.String())

	var ready []io.ReadWriteCloser
	for i, conn := range muxConns {
		if authToken != "" {
			if err := mux.WriteAuthToken(conn, authToken); err != nil {
				logger.Warn("write auth token failed (telemost)", "index", i, "err", err)
				conn.Close()
				continue
			}
		}
		if err := mux.WriteSessionID(conn, sid); err != nil {
			logger.Warn("write session ID failed (telemost)", "index", i, "err", err)
			conn.Close()
			continue
		}
		ready = append(ready, conn)
	}
	muxConns = ready
	if len(muxConns) == 0 {
		logger.Error("all Telemost connections failed handshake")
		os.Exit(1)
	}

	m := mux.New(logger, muxConns...)
	defer m.Close()

	logger.Info("MUX ready (Telemost)", "active", m.ActiveConns(), "target", numConns)

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 10*time.Second)

	startProxies(ctx, logger, siren, m, len(muxConns), numConns, socks5Port, httpPort, bindAddr, bypassMatcher)
}

// runDirect connects through TURN to a server listening on a direct UDP address.
func runDirect(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	svc provider.Service, server string, numConns int, useTCP bool, socks5Port, httpPort int, bindAddr, authToken string,
	bypassMatcher *bypass.Matcher) {

	serverUDPAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		logger.Error("invalid server address", "addr", server, "err", err)
		os.Exit(1)
	}

	sessionID := uuid.New()
	logger.Info("session (direct mode)", "id", sessionID.String())

	// 1. Create TURN allocations.
	logger.Info("establishing TURN connections", "count", numConns, "service", svc.Name())
	mgr := turn.NewManager(svc, useTCP, logger)
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
		logger.Info("DTLS connection established", "index", i, "progress", fmt.Sprintf("%d/%d", len(muxConns), numConns))
	}

	if len(muxConns) == 0 {
		logger.Error("no DTLS connections established")
		os.Exit(1)
	}

	// 3. Create multiplexer over DTLS connections.
	m := mux.New(logger, muxConns...)
	defer m.Close()

	logger.Info("MUX ready", "active", m.ActiveConns(), "target", numConns)

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 10*time.Second)

	// 4. Start proxies.
	startProxies(ctx, logger, siren, m, len(muxConns), numConns, socks5Port, httpPort, bindAddr, bypassMatcher)
}

// relaySession holds the state of a single relay-to-relay session.
// Fields are protected by mu for safe replacement during full reconnect.
type relaySession struct {
	mu        sync.Mutex
	sigClient provider.SignalingClient
	mgr       *turn.Manager
	m         *mux.Mux
	sessionID uuid.UUID
	cleanups  []context.CancelFunc
}

// Mux returns the current MUX, safe for concurrent use.
func (rs *relaySession) Mux() *mux.Mux {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.m
}

// Close tears down all resources of this relay session.
func (rs *relaySession) Close() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.m != nil {
		rs.m.Close()
	}
	for _, c := range rs.cleanups {
		c()
	}
	if rs.mgr != nil {
		rs.mgr.CloseAll()
	}
	if rs.sigClient != nil {
		rs.sigClient.Close()
	}
}

// connectRelaySession establishes a full relay-to-relay session:
// join conference → signaling → TURN allocations → relay addr exchange → DTLS.
func connectRelaySession(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	svc provider.Service, numConns int, useTCP bool, authToken string) (*relaySession, error) {

	// 1. Join conference to get TURN creds and signaling endpoint.
	jr, err := svc.FetchJoinInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("join conference: %w", err)
	}
	logger.Info("joined conference", "ws_endpoint", jr.WSEndpoint, "conv_id", jr.ConvID)

	// 2. Connect to signaling.
	sigClient, err := svc.ConnectSignaling(ctx, jr, logger.With("component", "signaling"))
	if err != nil {
		return nil, fmt.Errorf("signaling connect: %w", err)
	}

	if err := sigClient.SetKey(authToken); err != nil {
		sigClient.Close()
		return nil, fmt.Errorf("set signaling key: %w", err)
	}

	// Tell server to kill any existing session so it's ready for us.
	_ = sigClient.SendDisconnect(ctx)

	// 3. Create TURN allocations.
	mgr := turn.NewManager(svc, useTCP, logger)

	allocs, err := mgr.Allocate(ctx, numConns)
	if err != nil {
		siren.AlertTURNAuthFailure(ctx, err)
		sigClient.Close()
		mgr.CloseAll()
		return nil, fmt.Errorf("allocate TURN: %w", err)
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
		sigClient.Close()
		mgr.CloseAll()
		return nil, fmt.Errorf("recv relay addrs: %w", err)
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

	// 5. Punch relay and establish DTLS in parallel.
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
	var muxConns []io.ReadWriteCloser
	for j := 0; j < pairCount; j++ {
		r := <-results
		if r.err != nil {
			logger.Warn("relay DTLS failed", "index", r.index, "err", r.err)
			continue
		}
		cleanups = append(cleanups, r.cleanup)
		muxConns = append(muxConns, r.conn)
		logger.Info("relay DTLS connection established", "index", r.index, "progress", fmt.Sprintf("%d/%d", len(muxConns), pairCount))
	}
	punchCancel()

	if len(muxConns) == 0 {
		sigClient.Close()
		mgr.CloseAll()
		for _, c := range cleanups {
			c()
		}
		return nil, fmt.Errorf("no relay DTLS connections established")
	}

	m := mux.New(logger, muxConns...)
	return &relaySession{
		sigClient: sigClient,
		mgr:       mgr,
		m:         m,
		sessionID: sessionID,
		cleanups:  cleanups,
	}, nil
}

// runRelayToRelay connects through TURN relays to a server that also
// joins the same call. Relay addresses are exchanged via signaling.
func runRelayToRelay(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	svc provider.Service, numConns int, useTCP bool, socks5Port, httpPort int, bindAddr, authToken string,
	bypassMatcher *bypass.Matcher) {

	logger.Info("starting relay-to-relay mode", "service", svc.Name(), "conns", numConns)

	sess, err := connectRelaySession(ctx, logger, siren, svc, numConns, useTCP, authToken)
	if err != nil {
		logger.Error("failed to establish relay session", "err", err)
		os.Exit(1)
	}
	defer sess.Close()

	logger.Info("tunnel connected (relay-to-relay)",
		"active", sess.m.ActiveConns(), "target", numConns,
		"session_id", sess.sessionID.String())

	go sess.m.DispatchLoop(ctx)
	go sess.m.StartPingLoop(ctx, 10*time.Second)
	go sess.mgr.StartKeepalive(ctx, 10*time.Second)

	// Start unified reconnect manager (handles per-conn reconnect + full session reconnect).
	fullReconnect := make(chan struct{}, 1)

	go reconnectManager(ctx, sess, fullReconnect, authToken, numConns, logger)

	// Monitor signaling for session end — trigger full reconnect on hungup.
	go func() {
		reason := sess.sigClient.WaitForSessionEnd(ctx)
		if reason == provider.SessionEndHungup {
			logger.Warn("VK terminated the call (hungup), triggering full session reconnect")
			select {
			case fullReconnect <- struct{}{}:
			default:
			}
		}
	}()

	// Full session reconnect loop: when signaling dies or VK hangs up,
	// tear down everything and re-establish from scratch.
	go func() {
		const maxBackoff = 60 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			case <-fullReconnect:
			}

			logger.Info("starting full session reconnect")

			// Tear down old session resources (except MUX — it stays for proxy continuity
			// until new session is ready, but we close signaling + TURN).
			sess.sigClient.Close()
			sess.mgr.CloseAll()

			backoff := time.Second
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}

				newSess, err := connectRelaySession(ctx, logger, siren, svc, numConns, useTCP, authToken)
				if err != nil {
					logger.Warn("full session reconnect failed", "err", err, "next_backoff", backoff)
					backoff = backoff * 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
					continue
				}

				// Replace old session atomically.
				sess.mu.Lock()
				oldM := sess.m
				oldCleanups := sess.cleanups
				sess.sigClient = newSess.sigClient
				sess.mgr = newSess.mgr
				sess.m = newSess.m
				sess.sessionID = newSess.sessionID
				sess.cleanups = newSess.cleanups
				sess.mu.Unlock()

				// Close old MUX and cancel old bridge goroutines.
				// Note: old sigClient and mgr were already closed before the retry loop.
				oldM.Close()
				for _, c := range oldCleanups {
					c()
				}

				logger.Info("full session reconnect succeeded",
					"active", sess.m.ActiveConns(), "target", numConns,
					"session_id", sess.sessionID.String())

				go sess.m.DispatchLoop(ctx)
				go sess.m.StartPingLoop(ctx, 10*time.Second)
				go sess.mgr.StartKeepalive(ctx, 10*time.Second)

				// Restart reconnect manager for new session.
				go reconnectManager(ctx, sess, fullReconnect, authToken, numConns, logger)

				// Restart hungup monitor for new session.
				go func() {
					reason := sess.sigClient.WaitForSessionEnd(ctx)
					if reason == provider.SessionEndHungup {
						logger.Warn("VK terminated the call (hungup), triggering full session reconnect")
						select {
						case fullReconnect <- struct{}{}:
						default:
						}
					}
				}()
				break
			}
		}
	}()

	// Start proxies (blocks until ctx done).
	// dialMux returns the current MUX, following full session reconnects.
	dialMux := func() *mux.Mux { return sess.Mux() }
	startRelayProxies(ctx, logger, siren, dialMux, numConns, socks5Port, httpPort, bindAddr, bypassMatcher)
}

// reconnectManager is a unified, serialized reconnect loop.
// It handles per-connection reconnects when signaling is alive,
// and triggers a full session reconnect when signaling is dead.
func reconnectManager(ctx context.Context, sess *relaySession, fullReconnect chan struct{},
	authToken string, targetConns int, logger *slog.Logger) {

	sigClient := sess.sigClient
	mgr := sess.mgr
	m := sess.m
	sessionID := sess.sessionID

	// Local context: cancelled when this reconnectManager returns,
	// so the ConnDied drain goroutine doesn't leak after full session reconnect.
	localCtx, localCancel := context.WithCancel(ctx)
	defer localCancel()

	ackCh, unsub := sigClient.Subscribe(internalsignal.WireConnOk, 8)
	defer unsub()

	minActive := targetConns / 2
	if minActive < 2 {
		minActive = 2
	}
	if minActive > targetConns {
		minActive = targetConns
	}

	const healthTimeout = 10 * time.Second

	// Wakeup signal: triggered on connection death for fast response.
	wakeup := make(chan struct{}, 1)
	triggerWakeup := func() {
		select {
		case wakeup <- struct{}{}:
		default:
		}
	}

	// Drain ConnDied and trigger wakeup.
	go func() {
		for {
			select {
			case <-localCtx.Done():
				return
			case idx, ok := <-m.ConnDied():
				if !ok {
					return
				}
				m.RemoveConn(idx)
				// Log allocation age if available.
				allocs := mgr.Allocations()
				var ageStr string
				if idx < len(allocs) && allocs[idx] != nil {
					ageStr = time.Since(allocs[idx].CreatedAt).Round(time.Second).String()
				}
				logger.Info("connection died", "index", idx, "allocation_age", ageStr)
				triggerWakeup()
			}
		}
	}()

	// Unified loop: check every 2s or on wakeup, maintain target connection count.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	const (
		normalMaxAttempts = 5
		maxBackoff        = 30 * time.Second
	)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wakeup:
		}

		// If signaling is dead, per-conn reconnect is impossible — trigger full session reconnect.
		if !sigClient.IsAlive() {
			logger.Warn("signaling connection dead, triggering full session reconnect")
			select {
			case fullReconnect <- struct{}{}:
			default:
			}
			return // this reconnectManager instance exits; a new one starts after full reconnect
		}

		active := m.ActiveConns()
		healthy := m.IsHealthy(healthTimeout)
		if active >= targetConns && healthy {
			continue
		}

		critical := active < minActive || !healthy
		needed := targetConns - active
		if needed <= 0 {
			// All conns alive but unhealthy — probe and force reconnect of 1.
			needed = 1
			m.ProbeConnections(3 * time.Second)
			logger.Warn("tunnel unhealthy, no pong received",
				"active", active, "healthy", healthy)
			// Wait for probe to detect dead connections.
			select {
			case <-ctx.Done():
				return
			case <-time.After(4 * time.Second):
			}
			active = m.ActiveConns()
			needed = targetConns - active
			if needed <= 0 {
				continue
			}
		}
		logger.Info("connections below target, reconnecting",
			"active", active, "target", targetConns, "needed", needed,
			"min_active", minActive, "critical", critical, "healthy", healthy)

		for i := 0; i < needed; i++ {
			m.BeginReconnect()
			var err error
			backoff := time.Second
			for attempt := 1; ; attempt++ {
				// Re-check signaling before each attempt.
				if !sigClient.IsAlive() {
					logger.Warn("signaling died during reconnect, triggering full session reconnect")
					m.EndReconnect()
					select {
					case fullReconnect <- struct{}{}:
					default:
					}
					return
				}

				err = reconnectOne(ctx, sigClient, mgr, m, ackCh, sessionID, authToken, logger)
				if err == nil {
					break
				}

				if !critical && attempt >= normalMaxAttempts {
					logger.Warn("reconnect failed, will retry next cycle",
						"attempts", attempt, "err", err)
					break
				}

				logger.Warn("reconnect attempt failed",
					"attempt", attempt, "err", err, "critical", critical,
					"next_backoff", backoff)
				select {
				case <-ctx.Done():
					m.EndReconnect()
					return
				case <-time.After(backoff):
				}
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}

				// Re-check if still critical (other reconnects may have succeeded).
				critical = m.ActiveConns() < minActive || !m.IsHealthy(healthTimeout)
			}
			m.EndReconnect()
			if err != nil && !critical {
				break // normal mode: stop, wait for next tick
			}
		}
	}
}

func reconnectOne(ctx context.Context, sigClient provider.SignalingClient,
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
	time.Sleep(200 * time.Millisecond)

	reconnCtx, reconnCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reconnCancel()
	dtlsConn, _, err := internaldtls.DialOverTURN(reconnCtx, alloc.RelayConn, serverUDP)
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
	logger.Info("reconnect: new connection added to MUX",
		"active", m.ActiveConns(),
		"total", m.TotalConns(),
	)
	return nil
}

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
