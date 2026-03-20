package client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/call-vpn/call-vpn/internal/bypass"
	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

// Config holds all parameters needed by the client run modes.
type Config struct {
	Service       provider.Service
	ServerAddr    string // empty = relay-to-relay mode
	NumConns      int
	UseTCP        bool
	AuthToken     string
	Fingerprint   []byte // DTLS cert fingerprint (nil = no pinning)
	SocksPort     int
	HTTPPort      int
	BindAddr      string
	BypassMatcher *bypass.Matcher
	Logger        *slog.Logger
	Siren         *monitoring.Siren
}

// RunTelemost connects through Telemost's Goloom SFU via WebRTC DataChannels.
// Both client and server join the same Telemost meeting; the SFU forwards
// DataChannel data between participants.
func RunTelemost(ctx context.Context, cfg *Config) {
	logger := cfg.Logger
	svc := cfg.Service.(*telemost.Service)

	logger.Info("starting Telemost WebRTC mode", "service", svc.Name(), "conns", cfg.NumConns)

	// Derive deterministic display names from auth token for 1:1 pairing.
	serverNames, clientNames := provider.DeriveDisplayNames(cfg.AuthToken, cfg.NumConns)

	var muxConns []io.ReadWriteCloser
	var cleanups []func()

	for i := 0; i < cfg.NumConns; i++ {
		myName := clientNames[i]
		peerName := serverNames[i]
		conn, cleanup, err := svc.ConnectPaired(ctx, logger.With("index", i), myName, peerName, i)
		if err != nil {
			logger.Warn("Telemost WebRTC connection failed", "index", i, "err", err)
			continue
		}
		muxConns = append(muxConns, conn)
		cleanups = append(cleanups, cleanup)
		logger.Info("Telemost WebRTC connection established", "index", i, "progress", fmt.Sprintf("%d/%d", len(muxConns), cfg.NumConns))
	}

	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	if len(muxConns) == 0 {
		logger.Error("no Telemost connections established")
		return
	}

	// Send auth token + session UUID on each connection before MUX takes over.
	sessionID := uuid.New()
	var sid [16]byte
	copy(sid[:], sessionID[:])
	logger.Info("session (Telemost)", "id", sessionID.String())

	var ready []io.ReadWriteCloser
	for i, conn := range muxConns {
		if cfg.AuthToken != "" {
			if err := mux.WriteAuthToken(conn, cfg.AuthToken); err != nil {
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
		return
	}

	m := mux.New(logger, muxConns...)
	defer m.Close()

	logger.Info("MUX ready (Telemost)", "active", m.ActiveConns(), "target", cfg.NumConns)

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 10*time.Second)

	startProxies(ctx, logger, cfg.Siren, m, len(muxConns), cfg.NumConns, cfg.SocksPort, cfg.HTTPPort, cfg.BindAddr, cfg.BypassMatcher)
}

// RunDirect connects through TURN to a server listening on a direct UDP address.
func RunDirect(ctx context.Context, cfg *Config) {
	logger := cfg.Logger

	serverUDPAddr, err := net.ResolveUDPAddr("udp", cfg.ServerAddr)
	if err != nil {
		logger.Error("invalid server address", "addr", cfg.ServerAddr, "err", err)
		return
	}

	sessionID := uuid.New()
	logger.Info("session (direct mode)", "id", sessionID.String())

	// 1. Create TURN allocations.
	logger.Info("establishing TURN connections", "count", cfg.NumConns, "service", cfg.Service.Name())
	mgr := turn.NewManager(cfg.Service, cfg.UseTCP, logger)
	defer mgr.CloseAll()

	allocs, err := mgr.Allocate(ctx, cfg.NumConns)
	if err != nil {
		cfg.Siren.AlertTURNAuthFailure(ctx, err)
		logger.Error("failed to allocate TURN connections", "err", err)
		return
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
		dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, alloc.RelayConn, serverUDPAddr, cfg.Fingerprint)
		if err != nil {
			logger.Warn("DTLS-over-TURN failed", "index", i, "err", err)
			continue
		}
		cleanups = append(cleanups, cleanup)

		// Send auth token if configured.
		if cfg.AuthToken != "" {
			if err := mux.WriteAuthToken(dtlsConn, cfg.AuthToken); err != nil {
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
		logger.Info("DTLS connection established", "index", i, "progress", fmt.Sprintf("%d/%d", len(muxConns), cfg.NumConns))
	}

	if len(muxConns) == 0 {
		logger.Error("no DTLS connections established")
		return
	}

	// 3. Create multiplexer over DTLS connections.
	m := mux.New(logger, muxConns...)
	defer m.Close()

	logger.Info("MUX ready", "active", m.ActiveConns(), "target", cfg.NumConns)

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 10*time.Second)

	// 4. Start proxies.
	startProxies(ctx, logger, cfg.Siren, m, len(muxConns), cfg.NumConns, cfg.SocksPort, cfg.HTTPPort, cfg.BindAddr, cfg.BypassMatcher)
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
// join conference -> signaling -> TURN allocations -> relay addr exchange -> DTLS.
func connectRelaySession(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren,
	svc provider.Service, numConns int, useTCP bool, authToken string, expectedFP []byte) (*relaySession, error) {

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

			dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, relayConn, addr, expectedFP)
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

// RunRelayToRelay connects through TURN relays to a server that also
// joins the same call. Relay addresses are exchanged via signaling.
func RunRelayToRelay(ctx context.Context, cfg *Config) {
	logger := cfg.Logger

	logger.Info("starting relay-to-relay mode", "service", cfg.Service.Name(), "conns", cfg.NumConns)

	sess, err := connectRelaySession(ctx, logger, cfg.Siren, cfg.Service, cfg.NumConns, cfg.UseTCP, cfg.AuthToken, cfg.Fingerprint)
	if err != nil {
		logger.Error("failed to establish relay session", "err", err)
		return
	}
	defer sess.Close()

	logger.Info("tunnel connected (relay-to-relay)",
		"active", sess.m.ActiveConns(), "target", cfg.NumConns,
		"session_id", sess.sessionID.String())

	go sess.m.DispatchLoop(ctx)
	go sess.m.StartPingLoop(ctx, 10*time.Second)
	go sess.mgr.StartKeepalive(ctx, 10*time.Second)

	// Start unified reconnect manager (handles per-conn reconnect + full session reconnect).
	fullReconnect := make(chan struct{}, 1)

	go NewReconnectManager(ReconnectConfig{
		TargetConns:     cfg.NumConns,
		AuthToken:       cfg.AuthToken,
		Fingerprint:     cfg.Fingerprint,
		SessionID:       sess.sessionID,
		Logger:          logger,
		OnFullReconnect: func() {
			select {
			case fullReconnect <- struct{}{}:
			default:
			}
		},
	}, sess.sigClient, sess.mgr, sess.m).Run(ctx)

	// Monitor signaling for session end -- trigger full reconnect on hungup.
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

			// Tear down old session resources (except MUX -- it stays for proxy continuity
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

				newSess, err := connectRelaySession(ctx, logger, cfg.Siren, cfg.Service, cfg.NumConns, cfg.UseTCP, cfg.AuthToken, cfg.Fingerprint)
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
					"active", sess.m.ActiveConns(), "target", cfg.NumConns,
					"session_id", sess.sessionID.String())

				go sess.m.DispatchLoop(ctx)
				go sess.m.StartPingLoop(ctx, 10*time.Second)
				go sess.mgr.StartKeepalive(ctx, 10*time.Second)

				// Restart reconnect manager for new session.
				go NewReconnectManager(ReconnectConfig{
					TargetConns:     cfg.NumConns,
					AuthToken:       cfg.AuthToken,
					Fingerprint:     cfg.Fingerprint,
					SessionID:       sess.sessionID,
					Logger:          logger,
					OnFullReconnect: func() {
						select {
						case fullReconnect <- struct{}{}:
						default:
						}
					},
				}, sess.sigClient, sess.mgr, sess.m).Run(ctx)

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
	startRelayProxies(ctx, logger, cfg.Siren, dialMux, cfg.NumConns, cfg.SocksPort, cfg.HTTPPort, cfg.BindAddr, cfg.BypassMatcher)
}

