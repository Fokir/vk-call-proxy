package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/call-vpn/call-vpn/internal/tunnel"
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
	VKTokens      []string // VK account tokens for authenticated TURN credential flow
	Fingerprint   []byte   // DTLS cert fingerprint (nil = no pinning)
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
	serverNames, clientNames := telemost.DeriveDisplayNames(cfg.AuthToken, cfg.NumConns)

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
	svc provider.Service, numConns int, useTCP bool, authToken string, expectedFP []byte, vkTokens []string) (*relaySession, error) {

	if authToken == "" {
		return nil, fmt.Errorf("token is required for relay-to-relay mode")
	}

	// 1. Join conference to get TURN creds and signaling endpoint.
	// Use token-authenticated join when VK tokens are available to avoid
	// anonymous VK API calls (getAnonymousAccessTokenPayload) and rate limits.
	var jr *provider.JoinInfo
	var err error
	if tap, ok := svc.(provider.TokenAuthProvider); ok && len(vkTokens) > 0 {
		jr, err = tap.FetchJoinInfoWithToken(ctx, vkTokens[0])
		if err != nil {
			logger.Warn("token join failed, falling back to anonymous", "err", err)
			jr, err = svc.FetchJoinInfo(ctx)
		}
	} else {
		jr, err = svc.FetchJoinInfo(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("join conference: %w", err)
	}
	logger.Info("joined conference", "ws_endpoint", jr.WSEndpoint, "conv_id", jr.ConvID)

	// 2. Setup signaling (connect + SetKey + disconnect handshake + keepalive).
	sigClient, err := svc.ConnectSignaling(ctx, jr, logger.With("component", "signaling"))
	if err != nil {
		return nil, fmt.Errorf("signaling connect: %w", err)
	}

	if err := sigClient.SetKey(authToken); err != nil {
		sigClient.Close()
		return nil, fmt.Errorf("set signaling key: %w", err)
	}

	// Send accept-call immediately to prevent VK from kicking us as idle.
	// Must be before disconnect handshake (which takes 6s) — authenticated
	// participants get kicked after ~7s without accept-call.
	if ac, ok := sigClient.(interface{ SendAcceptCall() error }); ok {
		if err := ac.SendAcceptCall(); err != nil {
			logger.Warn("failed to send accept-call", "err", err)
		}
	}

	// Generate session nonce for filtering ghost messages.
	nonceBytes := make([]byte, 8)
	rand.Read(nonceBytes)
	nonce := hex.EncodeToString(nonceBytes)

	// Tell server to kill any existing session (disconnect-req + ack handshake).
	const disconnectRetries = 3
	for i := 0; i < disconnectRetries; i++ {
		_ = sigClient.SendDisconnectReq(ctx, nonce)
		ackCtx, ackCancel := context.WithTimeout(ctx, 2*time.Second)
		err := sigClient.WaitDisconnectAck(ackCtx, nonce)
		ackCancel()
		if err == nil {
			logger.Info("disconnect ack received", "nonce", nonce)
			break
		}
		logger.Debug("disconnect ack timeout, retrying", "attempt", i+1, "nonce", nonce)
	}

	// Start signaling keepalive AFTER disconnect handshake to prevent VK
	// from kicking us as idle. Must be after disconnect to avoid race.
	if ka, ok := sigClient.(interface {
		StartSignalingKeepAlive(context.Context, *slog.Logger)
	}); ok {
		ka.StartSignalingKeepAlive(ctx, logger)
	}

	// 3. Start batched TURN allocations.
	mgr := turn.NewManager(svc, useTCP, logger)
	// Reuse credentials from initial join to avoid re-joining the conference
	// (VK hangs up the signaling session when a new anonymous participant joins).
	mgr.SetInitialCredentials(&jr.Credentials)

	// 4. Process batches: allocate → signaling → DTLS → MUX.
	sessionID := uuid.New()
	logger.Info("session (relay-to-relay mode)", "id", sessionID.String())

	var cleanups []context.CancelFunc
	var m *mux.Mux

	sigClient.StartPunchDispatcher(ctx, nonce)

	type dtlsResult struct {
		conn    io.ReadWriteCloser
		cleanup context.CancelFunc
		err     error
	}

	batchIdx := 0
	punchIdx := 0 // global punch index across all batches

	// Deduplicate and cap tokens.
	vkTokens = deduplicateTokens(vkTokens, logger)
	if len(vkTokens) > numConns {
		vkTokens = vkTokens[:numConns]
	}
	tokenConns := len(vkTokens)
	anonConns := numConns - tokenConns

	// Token allocation phase: allocate all tokens in parallel, send as batch 0.
	// For the first token (index 0): reuse initial join credentials to avoid
	// re-joining the conference (VK hangs up on duplicate joins).
	if tokenConns > 0 {
		tokenAllocs := make([]*turn.Allocation, tokenConns)
		var tokenWg sync.WaitGroup
		var tokenMu sync.Mutex
		for i, tok := range vkTokens {
			tokenWg.Add(1)
			go func(idx int, token string) {
				defer tokenWg.Done()
				var alloc *turn.Allocation
				var aErr error
				if idx == 0 {
					// First token was already used for initial join — reuse those credentials.
					alloc, aErr = mgr.AllocateWithCredentials(ctx, &jr.Credentials)
				} else {
					alloc, aErr = allocateWithToken(ctx, svc, mgr, token)
				}
				tokenMu.Lock()
				defer tokenMu.Unlock()
				if aErr != nil {
					logger.Warn("token allocation failed", "index", idx, "err", aErr)
					return
				}
				tokenAllocs[idx] = alloc
			}(i, tok)
		}
		tokenWg.Wait()

		// Collect successful allocations.
		var tokenAddrs []string
		var successAllocs []*turn.Allocation
		for _, a := range tokenAllocs {
			if a != nil {
				tokenAddrs = append(tokenAddrs, a.RelayAddr.String())
				successAllocs = append(successAllocs, a)
			}
		}

		if len(tokenAddrs) > 0 {
			final := anonConns == 0
			err = sigClient.SendRelayBatch(ctx, tokenAddrs, "client", nonce, batchIdx, final)
			if err != nil {
				mgr.CloseAll()
				sigClient.Close()
				return nil, fmt.Errorf("send token relay batch: %w", err)
			}
			serverAddrs, _, _, _, err := sigClient.RecvRelayBatch(ctx, "client", nonce)
			if err != nil {
				mgr.CloseAll()
				sigClient.Close()
				return nil, fmt.Errorf("recv token relay batch: %w", err)
			}

			pairCount := len(successAllocs)
			if len(serverAddrs) < pairCount {
				pairCount = len(serverAddrs)
			}

			// DTLS handshake for each pair in this batch (parallel).
			results := make(chan dtlsResult, pairCount)
			punchCtx, punchCancel := context.WithCancel(ctx)

			for i := 0; i < pairCount; i++ {
				currentPunchIdx := punchIdx + i
				serverUDP, rErr := net.ResolveUDPAddr("udp", serverAddrs[i])
				if rErr != nil {
					logger.Warn("resolve server relay addr", "batch", batchIdx, "index", i, "addr", serverAddrs[i], "err", rErr)
					results <- dtlsResult{err: rErr}
					continue
				}
				go func(relayConn net.PacketConn, addr *net.UDPAddr, pIdx int) {
					var dtlsConn net.Conn
					var cleanup context.CancelFunc
					var lastErr error
					for attempt := 1; attempt <= 2; attempt++ {
						punchLoopCtx, punchLoopCancel := context.WithCancel(punchCtx)
						internaldtls.PunchRelay(relayConn, addr)
						go internaldtls.StartPunchLoop(punchLoopCtx, relayConn, addr)

						punchReadyCtx, prc := context.WithTimeout(ctx, 3*time.Second)
						waitPunch := sigClient.PreparePunchWait(punchReadyCtx, nonce, pIdx)
						_ = sigClient.SendPunchReady(ctx, nonce, pIdx)
						_ = waitPunch()
						prc()

						dtlsConn, cleanup, lastErr = internaldtls.DialOverTURN(ctx, relayConn, addr, expectedFP)
						punchLoopCancel()

						if lastErr == nil {
							break
						}
						logger.Warn("DTLS handshake failed", "attempt", attempt, "punch_idx", pIdx, "err", lastErr)
						if attempt < 2 {
							time.Sleep(time.Duration(attempt) * time.Second)
						}
					}
					if lastErr != nil {
						results <- dtlsResult{err: lastErr}
						return
					}

					if authToken != "" {
						if wErr := mux.WriteAuthToken(dtlsConn, authToken); wErr != nil {
							cleanup()
							results <- dtlsResult{err: fmt.Errorf("write auth token: %w", wErr)}
							return
						}
					}

					var sid [16]byte
					copy(sid[:], sessionID[:])
					if wErr := mux.WriteSessionID(dtlsConn, sid); wErr != nil {
						cleanup()
						results <- dtlsResult{err: fmt.Errorf("write session id: %w", wErr)}
						return
					}

					results <- dtlsResult{conn: dtlsConn, cleanup: cleanup}
				}(successAllocs[i].RelayConn, serverUDP, currentPunchIdx)
			}

			for j := 0; j < pairCount; j++ {
				r := <-results
				if r.err != nil {
					logger.Warn("relay DTLS failed in batch", "batch", batchIdx, "err", r.err)
					continue
				}
				cleanups = append(cleanups, r.cleanup)
				if m == nil {
					m = mux.New(logger, r.conn)
					logger.Info("first relay connection ready, MUX created", "batch", batchIdx)
				} else {
					m.AddConn(r.conn)
				}
			}
			punchCancel()

			punchIdx += pairCount
		}

		// Failed tokens become anonymous.
		failedTokens := tokenConns - len(successAllocs)
		anonConns += failedTokens
		batchIdx++
	}

	// Anonymous TURN allocations (skip if all connections handled by tokens).
	if anonConns > 0 {
		batchCh := mgr.AllocateGradual(ctx, anonConns, turn.GradualOpts{})

		for br := range batchCh {
			if len(br.Allocs) == 0 {
				if br.Final {
					break
				}
				continue
			}

			// Collect relay addresses for this batch.
			addrs := make([]string, len(br.Allocs))
			for i, a := range br.Allocs {
				addrs[i] = a.RelayAddr.String()
			}
			logger.Info("batch ready", "batch", batchIdx, "allocs", len(br.Allocs), "final", br.Final)

			// Exchange batch addresses with server via signaling.
			if err := sigClient.SendRelayBatch(ctx, addrs, "client", nonce, batchIdx, br.Final); err != nil {
				logger.Error("send relay batch", "batch", batchIdx, "err", err)
				break
			}
			serverAddrs, _, _, _, err := sigClient.RecvRelayBatch(ctx, "client", nonce)
			if err != nil {
				logger.Error("recv relay batch", "batch", batchIdx, "err", err)
				break
			}

			pairCount := len(br.Allocs)
			if len(serverAddrs) < pairCount {
				pairCount = len(serverAddrs)
			}

			// DTLS handshake for each pair in this batch (parallel).
			results := make(chan dtlsResult, pairCount)
			punchCtx, punchCancel := context.WithCancel(ctx)

			for i := 0; i < pairCount; i++ {
				currentPunchIdx := punchIdx + i
				serverUDP, err := net.ResolveUDPAddr("udp", serverAddrs[i])
				if err != nil {
					logger.Warn("resolve server relay addr", "batch", batchIdx, "index", i, "addr", serverAddrs[i], "err", err)
					results <- dtlsResult{err: err}
					continue
				}
				go func(relayConn net.PacketConn, addr *net.UDPAddr, pIdx int) {
					var dtlsConn net.Conn
					var cleanup context.CancelFunc
					var lastErr error
					for attempt := 1; attempt <= 2; attempt++ {
						punchLoopCtx, punchLoopCancel := context.WithCancel(punchCtx)
						internaldtls.PunchRelay(relayConn, addr)
						go internaldtls.StartPunchLoop(punchLoopCtx, relayConn, addr)

						punchReadyCtx, prc := context.WithTimeout(ctx, 3*time.Second)
						waitPunch := sigClient.PreparePunchWait(punchReadyCtx, nonce, pIdx)
						_ = sigClient.SendPunchReady(ctx, nonce, pIdx)
						_ = waitPunch()
						prc()

						dtlsConn, cleanup, lastErr = internaldtls.DialOverTURN(ctx, relayConn, addr, expectedFP)
						punchLoopCancel()

						if lastErr == nil {
							break
						}
						logger.Warn("DTLS handshake failed", "attempt", attempt, "punch_idx", pIdx, "err", lastErr)
						if attempt < 2 {
							time.Sleep(time.Duration(attempt) * time.Second)
						}
					}
					if lastErr != nil {
						results <- dtlsResult{err: lastErr}
						return
					}

					if authToken != "" {
						if err := mux.WriteAuthToken(dtlsConn, authToken); err != nil {
							cleanup()
							results <- dtlsResult{err: fmt.Errorf("write auth token: %w", err)}
							return
						}
					}

					var sid [16]byte
					copy(sid[:], sessionID[:])
					if err := mux.WriteSessionID(dtlsConn, sid); err != nil {
						cleanup()
						results <- dtlsResult{err: fmt.Errorf("write session id: %w", err)}
						return
					}

					results <- dtlsResult{conn: dtlsConn, cleanup: cleanup}
				}(br.Allocs[i].RelayConn, serverUDP, currentPunchIdx)
			}

			for j := 0; j < pairCount; j++ {
				r := <-results
				if r.err != nil {
					logger.Warn("relay DTLS failed in batch", "batch", batchIdx, "err", r.err)
					continue
				}
				cleanups = append(cleanups, r.cleanup)
				if m == nil {
					m = mux.New(logger, r.conn)
					logger.Info("first relay connection ready, MUX created", "batch", batchIdx)
				} else {
					m.AddConn(r.conn)
				}
			}
			punchCancel()

			punchIdx += pairCount
			batchIdx++
			if br.Final {
				break
			}
		}
	}

	sigClient.StopPunchDispatcher()

	if m == nil {
		sigClient.Close()
		mgr.CloseAll()
		for _, c := range cleanups {
			c()
		}
		return nil, fmt.Errorf("no relay DTLS connections established")
	}

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

	sess, err := connectRelaySession(ctx, logger, cfg.Siren, cfg.Service, cfg.NumConns, cfg.UseTCP, cfg.AuthToken, cfg.Fingerprint, cfg.VKTokens)
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
		reason, _ := sess.sigClient.WaitForSessionEnd(ctx)
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

				newSess, err := connectRelaySession(ctx, logger, cfg.Siren, cfg.Service, cfg.NumConns, cfg.UseTCP, cfg.AuthToken, cfg.Fingerprint, cfg.VKTokens)
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
					reason, _ := sess.sigClient.WaitForSessionEnd(ctx)
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

// deduplicateTokens removes duplicate tokens, logging a warning for each.
func deduplicateTokens(tokens []string, logger *slog.Logger) []string {
	seen := make(map[string]bool, len(tokens))
	result := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if seen[t] {
			if len(t) > 20 {
				logger.Warn("duplicate VK token ignored", "token", t[:20]+"...")
			} else {
				logger.Warn("duplicate VK token ignored")
			}
			continue
		}
		seen[t] = true
		result = append(result, t)
	}
	return result
}

// allocateWithToken fetches TURN credentials using an authenticated token and creates a TURN allocation.
func allocateWithToken(ctx context.Context, svc provider.Service, mgr *turn.Manager, token string) (*turn.Allocation, error) {
	tap, ok := svc.(provider.TokenAuthProvider)
	if !ok {
		return nil, fmt.Errorf("provider does not support token auth")
	}
	info, err := tap.FetchJoinInfoWithToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("fetch join info with token: %w", err)
	}
	alloc, err := mgr.AllocateWithCredentials(ctx, &info.Credentials)
	if err != nil {
		return nil, fmt.Errorf("allocate with credentials: %w", err)
	}
	return alloc, nil
}

// MultiRelayConfig configures multi-call pool relay mode.
type MultiRelayConfig struct {
	Pool          *tunnel.CallPool
	SocksPort     int
	HTTPPort      int
	BindAddr      string
	BypassMatcher *bypass.Matcher
	Logger        *slog.Logger
	Siren         *monitoring.Siren
}

// RunMultiRelay starts the multi-call pool relay mode.
// The pool manages N parallel calls, each contributing connections to a shared MUX.
func RunMultiRelay(ctx context.Context, cfg *MultiRelayConfig) {
	logger := cfg.Logger

	m, err := cfg.Pool.StartClient(ctx)
	if err != nil {
		logger.Error("multi-call pool start failed", "err", err)
		return
	}

	logger.Info("multi-call tunnel connected", "active", m.ActiveConns())

	go m.DispatchLoop(ctx)
	go m.StartPingLoop(ctx, 10*time.Second)

	// Pool handles reconnect internally; just run proxies.
	dialMux := func() *mux.Mux { return cfg.Pool.Mux() }
	totalConns := 0
	for _, s := range cfg.Pool.Status() {
		totalConns += s.ActiveConn
	}
	startRelayProxies(ctx, logger, cfg.Siren, dialMux, totalConns, cfg.SocksPort, cfg.HTTPPort, cfg.BindAddr, cfg.BypassMatcher)
}

