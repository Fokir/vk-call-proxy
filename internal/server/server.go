package server

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/netstack"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	internalsignal "github.com/call-vpn/call-vpn/internal/signal"
	"github.com/call-vpn/call-vpn/internal/turn"
)

// Config holds server configuration.
type Config struct {
	ListenAddr string             // DTLS/UDP listen address for direct mode
	AuthToken  string             // Client authentication token
	Service    provider.Service   // Call service (VK, MAX, etc.) for relay-to-relay mode; nil = direct only
	UseTCP     bool               // Use TCP for TURN connections
	NumConns   int                // Number of parallel connections (Telemost mode)
	Logger     *slog.Logger
	Siren      *monitoring.Siren
}

// Server manages VPN server operations.
type Server struct {
	cfg Config

	cancel     context.CancelFunc
	done       chan struct{}
	sessionsMu sync.Mutex
	sessions   map[[16]byte]*session
}

type session struct {
	mu     sync.Mutex
	m      *mux.Mux
	logger *slog.Logger
	cancel context.CancelFunc
	conns  int
}

// SessionInfo holds public session statistics.
type SessionInfo struct {
	SessionID   string `json:"session_id"`
	ActiveConns int    `json:"active_conns"`
	TotalConns  int    `json:"total_conns"`
}

// GetSessionsInfo returns info about all active sessions.
func (s *Server) GetSessionsInfo() []SessionInfo {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()

	infos := make([]SessionInfo, 0, len(s.sessions))
	for id, sess := range s.sessions {
		sess.mu.Lock()
		info := SessionInfo{
			SessionID:   fmt.Sprintf("%x", id),
			ActiveConns: sess.m.ActiveConns(),
			TotalConns:  sess.conns,
		}
		sess.mu.Unlock()
		infos = append(infos, info)
	}
	return infos
}

// New creates a new Server instance.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Siren == nil {
		cfg.Siren = monitoring.New(cfg.Logger)
	}
	return &Server{
		cfg:      cfg,
		sessions: make(map[[16]byte]*session),
		done:     make(chan struct{}),
	}
}

// Start begins server operation in a goroutine.
func (s *Server) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	go func() {
		defer close(s.done)
		if s.cfg.Service != nil {
			if _, ok := s.cfg.Service.(*telemost.Service); ok {
				s.runTelemostMode(ctx)
			} else {
				s.runRelayMode(ctx)
			}
		} else {
			s.runDirectMode(ctx)
		}
	}()
}

// Stop signals the server to shut down and waits for completion.
func (s *Server) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		s.cfg.Logger.Warn("server stop timed out after 30s")
	}
}

// Done returns a channel that's closed when the server stops.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// --- Direct mode ---

func (s *Server) runDirectMode(ctx context.Context) {
	ln, err := internaldtls.Listen(s.cfg.ListenAddr)
	if err != nil {
		s.cfg.Logger.Error("failed to start DTLS listener", "err", err)
		return
	}
	defer ln.Close()

	fp := ln.CertFingerprint()
	s.cfg.Logger.Info("server listening (DTLS/UDP, direct mode)", "addr", s.cfg.ListenAddr,
		"fingerprint", hex.EncodeToString(fp[:]))

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
			s.cfg.Logger.Warn("accept error", "err", err)
			continue
		}
		go s.handleConnection(ctx, conn)
	}
}

func (s *Server) handleConnection(ctx context.Context, conn net.Conn) {
	if s.cfg.AuthToken != "" {
		if err := mux.ValidateAuthToken(conn, s.cfg.AuthToken); err != nil {
			s.cfg.Logger.Warn("auth failed", "remote", conn.RemoteAddr(), "err", err)
			conn.Close()
			return
		}
	}

	sessionID, err := mux.ReadSessionID(conn)
	if err != nil {
		s.cfg.Logger.Warn("read session id failed", "remote", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}

	s.cfg.Logger.Info("connection received",
		"remote", conn.RemoteAddr(),
		"session_id", fmt.Sprintf("%x", sessionID),
	)

	sess := s.getOrCreateSession(ctx, sessionID)

	sess.mu.Lock()
	sess.m.AddConn(conn)
	sess.conns++
	count := sess.conns
	sess.mu.Unlock()

	s.cfg.Logger.Info("connection added to session",
		"session_id", fmt.Sprintf("%x", sessionID),
		"total_conns", count,
		"active_conns", sess.m.ActiveConns(),
	)
}

func (s *Server) getOrCreateSession(ctx context.Context, id [16]byte) *session {
	s.sessionsMu.Lock()
	if sess, ok := s.sessions[id]; ok {
		s.sessionsMu.Unlock()
		return sess
	}

	sessCtx, sessCancel := context.WithCancel(ctx)
	sessLogger := s.cfg.Logger.With("session_id", fmt.Sprintf("%x", id))
	m := mux.New(sessLogger)

	m.EnableRawPackets(4096)
	m.EnableStreamAccept(64)
	m.SetIdleTimeout(90 * time.Second)
	m.SetMaxStreams(10000)

	sess := &session{
		m:      m,
		logger: sessLogger,
		cancel: sessCancel,
	}
	s.sessions[id] = sess
	s.sessionsMu.Unlock()

	go m.DispatchLoop(sessCtx)
	go m.StartPingLoop(sessCtx, 10*time.Second)

	ns := netstack.New(sessLogger, m)
	if ns != nil {
		ns.Start(sessCtx)
	}

	// Drain dead connections so their MUX slots can be reused.
	go func() {
		for {
			select {
			case idx, ok := <-m.ConnDied():
				if !ok {
					return
				}
				m.RemoveConn(idx)
			case <-sessCtx.Done():
				return
			}
		}
	}()

	go func() {
		defer func() {
			s.sessionsMu.Lock()
			delete(s.sessions, id)
			s.sessionsMu.Unlock()
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
				s.cfg.Siren.AlertDisconnect(sessCtx, fmt.Sprintf("session-%x", id))
				return
			case <-sessCtx.Done():
				return
			}
		}
	}()

	return sess
}

// --- Telemost mode ---

func (s *Server) runTelemostMode(ctx context.Context) {
	tmSvc := s.cfg.Service.(*telemost.Service)
	n := s.cfg.NumConns
	if n < 1 {
		n = 1
	}
	s.cfg.Logger.Info("starting Telemost WebRTC mode", "service", tmSvc.Name(), "conns", n)

	for {
		if ctx.Err() != nil {
			return
		}

		s.cfg.Logger.Info("waiting for client session (Telemost)...", "conns", n)
		err := s.runTelemostSession(ctx, tmSvc, n)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.cfg.Logger.Warn("Telemost session failed", "err", err)
		} else {
			s.cfg.Logger.Info("Telemost session ended")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// runTelemostSession joins the conference with N participants and groups
// them into a single MUX session via session UUID (same as VK relay mode).
func (s *Server) runTelemostSession(ctx context.Context, svc *telemost.Service, n int) error {
	type connResult struct {
		conn    io.ReadWriteCloser
		cleanup func()
		err     error
	}

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	// Derive deterministic display names from auth token.
	serverNames, clientNames := telemost.DeriveDisplayNames(s.cfg.AuthToken, n)

	// Phase 1: Setup all N participants in parallel (non-blocking).
	type setupResult struct {
		pc  *telemost.PendingConn
		err error
	}
	setupResults := make([]setupResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			pc, err := svc.SetupNamedIndexed(sessCtx, s.cfg.Logger.With("component", "telemost", "index", idx), serverNames[idx], idx)
			setupResults[idx] = setupResult{pc: pc, err: err}
		}(i)
	}
	wg.Wait()

	// Phase 2: Wait for client peers by name (via upsertDescription), pin, and connect.
	results := make([]connResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sr := setupResults[idx]
			if sr.err != nil {
				results[idx] = connResult{err: sr.err}
				return
			}
			conn, cleanup, err := sr.pc.WaitPinReady(sessCtx, s.cfg.Logger.With("component", "telemost", "index", idx), clientNames[idx])
			results[idx] = connResult{conn: conn, cleanup: cleanup, err: err}
		}(i)
	}
	wg.Wait()

	// Collect successful connections.
	var conns []io.ReadWriteCloser
	var cleanups []func()
	for i, r := range results {
		if r.err != nil {
			s.cfg.Logger.Warn("Telemost connect failed", "index", i, "err", r.err)
			continue
		}
		conns = append(conns, r.conn)
		cleanups = append(cleanups, r.cleanup)
		s.cfg.Logger.Info("Telemost WebRTC connection established", "index", i)
	}
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	if len(conns) == 0 {
		return fmt.Errorf("no Telemost connections established")
	}

	// For each connection: validate auth token, read session UUID, group into session.
	var firstSessionID [16]byte
	var sess *session
	var added atomic.Int32

	for i, conn := range conns {
		if s.cfg.AuthToken != "" {
			if err := mux.ValidateAuthToken(conn, s.cfg.AuthToken); err != nil {
				s.cfg.Logger.Warn("Telemost auth failed", "index", i, "err", err)
				conn.Close()
				continue
			}
		}

		sessionID, err := mux.ReadSessionID(conn)
		if err != nil {
			s.cfg.Logger.Warn("read session ID failed (telemost)", "index", i, "err", err)
			conn.Close()
			continue
		}

		if sess == nil {
			firstSessionID = sessionID
			sess = s.getOrCreateSession(sessCtx, sessionID)
		} else if sessionID != firstSessionID {
			s.cfg.Logger.Warn("session ID mismatch (telemost)", "index", i,
				"expected", fmt.Sprintf("%x", firstSessionID),
				"got", fmt.Sprintf("%x", sessionID))
			conn.Close()
			continue
		}

		sess.mu.Lock()
		sess.m.AddConn(conn)
		sess.conns++
		count := sess.conns
		sess.mu.Unlock()

		added.Add(1)
		s.cfg.Logger.Info("Telemost connection added to session",
			"index", i,
			"session_id", fmt.Sprintf("%x", firstSessionID),
			"total_conns", count,
		)
	}

	if added.Load() == 0 {
		return fmt.Errorf("no Telemost connections passed handshake")
	}

	s.cfg.Logger.Info("Telemost server ready", "active_conns", sess.m.ActiveConns())

	// Wait for session to end (getOrCreateSession manages stream accept loop).
	<-sess.m.Dead()
	return nil
}

// --- Relay mode ---

func (s *Server) runRelayMode(ctx context.Context) {
	s.cfg.Logger.Info("starting relay-to-relay mode", "service", s.cfg.Service.Name())

	for {
		if ctx.Err() != nil {
			return
		}

		s.cfg.Logger.Info("waiting for client session...")
		err := s.runOneRelaySession(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.cfg.Logger.Warn("relay session failed", "err", err)
		} else {
			s.cfg.Logger.Info("relay session ended")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (s *Server) runOneRelaySession(ctx context.Context) error {
	if s.cfg.AuthToken == "" {
		return fmt.Errorf("token is required for relay-to-relay mode")
	}

	svc := s.cfg.Service

	jr, err := svc.FetchJoinInfo(ctx)
	if err != nil {
		return fmt.Errorf("join conference: %w", err)
	}
	s.cfg.Logger.Info("joined conference", "service", svc.Name(), "ws_endpoint", jr.WSEndpoint, "conv_id", jr.ConvID)

	sigClient, err := svc.ConnectSignaling(ctx, jr, s.cfg.Logger.With("component", "signaling"))
	if err != nil {
		return fmt.Errorf("signaling connect: %w", err)
	}
	defer sigClient.Close()

	if err := sigClient.SetKey(s.cfg.AuthToken); err != nil {
		return fmt.Errorf("set signaling key: %w", err)
	}

	mgr := turn.NewManager(svc, s.cfg.UseTCP, s.cfg.Logger)
	defer mgr.CloseAll()

	clientAddrs, _, clientNonce, err := sigClient.RecvRelayAddrs(ctx, "server", "")
	if err != nil {
		return fmt.Errorf("recv relay addrs: %w", err)
	}
	s.cfg.Logger.Info("received client relay addresses", "count", len(clientAddrs), "nonce", clientNonce)

	pairCount := len(clientAddrs)
	allocs, err := mgr.Allocate(ctx, pairCount)
	if err != nil {
		s.cfg.Siren.AlertTURNAuthFailure(ctx, err)
		return fmt.Errorf("allocate TURN connections: %w", err)
	}
	s.cfg.Logger.Info("TURN allocations created on demand", "count", len(allocs))

	ourAddrs := make([]string, len(allocs))
	for i, a := range allocs {
		ourAddrs[i] = a.RelayAddr.String()
	}

	sendDone := make(chan struct{})
	sendCtx, sendCancel := context.WithCancel(ctx)
	go func() {
		defer close(sendDone)
		for {
			if err := sigClient.SendRelayAddrs(sendCtx, ourAddrs, "server", clientNonce); err != nil {
				return
			}
			select {
			case <-sendCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	go func() {
		time.Sleep(5 * time.Second)
		sendCancel()
		<-sendDone
	}()

	type dtlsResult struct {
		index   int
		conn    net.Conn
		cleanup context.CancelFunc
		err     error
	}
	results := make(chan dtlsResult, len(allocs))
	punchCtx, punchCancel := context.WithCancel(ctx)

	for i := 0; i < len(allocs); i++ {
		clientUDP, err := net.ResolveUDPAddr("udp", clientAddrs[i])
		if err != nil {
			s.cfg.Logger.Warn("resolve client relay addr", "index", i, "addr", clientAddrs[i], "err", err)
			results <- dtlsResult{index: i, err: err}
			continue
		}
		go func(idx int, relayConn net.PacketConn, addr *net.UDPAddr) {
			var dtlsConn net.Conn
			var cleanup context.CancelFunc
			var lastErr error
			for attempt := 1; attempt <= 2; attempt++ {
				punchLoopCtx, punchLoopCancel := context.WithCancel(punchCtx)
				internaldtls.PunchRelay(relayConn, addr)
				go internaldtls.StartPunchLoop(punchLoopCtx, relayConn, addr)
				time.Sleep(200 * time.Millisecond) // let TURN permissions establish

				dtlsConn, cleanup, lastErr = internaldtls.AcceptOverTURN(ctx, relayConn, addr)
				punchLoopCancel()

				if lastErr == nil {
					break
				}
				s.cfg.Logger.Warn("DTLS handshake failed", "attempt", attempt, "index", idx, "err", lastErr)
				if attempt < 2 {
					time.Sleep(time.Duration(attempt) * time.Second)
				}
			}
			results <- dtlsResult{index: idx, conn: dtlsConn, cleanup: cleanup, err: lastErr}
		}(i, allocs[i].RelayConn, clientUDP)
	}

	var cleanups []context.CancelFunc
	defer func() {
		for _, c := range cleanups {
			c()
		}
	}()

	m := mux.New(s.cfg.Logger)
	defer m.Close()

	m.EnableRawPackets(4096)
	m.EnableStreamAccept(64)
	m.SetIdleTimeout(90 * time.Second)
	m.SetMaxStreams(10000)

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	pingCtx, pingCancel := context.WithCancel(sessCtx)
	defer pingCancel()

	// Start MUX operations immediately so connections are served as
	// soon as they are added — don't wait for all handshakes.
	go m.DispatchLoop(sessCtx)
	go m.StartPingLoop(pingCtx, 10*time.Second)
	go mgr.StartKeepalive(sessCtx, 10*time.Second)

	ns := netstack.New(s.cfg.Logger, m)
	if ns != nil {
		ns.Start(sessCtx)
		defer ns.Close()
	}

	// Process DTLS handshake results as they arrive: validate auth +
	// session ID and add each connection to the MUX immediately.
	var added atomic.Int32
	var firstConn atomic.Bool
	var wg sync.WaitGroup
	wg.Add(pairCount)
	go func() {
		for j := 0; j < pairCount; j++ {
			r := <-results
			if r.err != nil {
				s.cfg.Logger.Warn("AcceptOverTURN failed", "index", r.index, "err", r.err)
				wg.Done()
				continue
			}
			cleanups = append(cleanups, r.cleanup)
			if firstConn.CompareAndSwap(false, true) {
				s.cfg.Logger.Info("first relay connection ready", "index", r.index)
			}
			go func(idx int, c net.Conn) {
				defer wg.Done()
				if s.cfg.AuthToken != "" {
					if err := mux.ValidateAuthToken(c, s.cfg.AuthToken); err != nil {
						s.cfg.Logger.Warn("auth failed on relay conn", "index", idx, "err", err)
						c.Close()
						return
					}
				}
				sessionID, err := mux.ReadSessionID(c)
				if err != nil {
					s.cfg.Logger.Warn("read session id failed on relay conn", "index", idx, "err", err)
					c.Close()
					return
				}
				s.cfg.Logger.Info("connection received",
					"index", idx,
					"session_id", fmt.Sprintf("%x", sessionID),
				)
				m.AddConn(c)
				added.Add(1)
			}(r.index, r.conn)
		}
	}()

	// Wait for at least one connection before proceeding to the session loop.
	// Use a short poll so we don't block the entire session on slow handshakes.
	waitCtx, waitCancel := context.WithTimeout(sessCtx, 60*time.Second)
	defer waitCancel()
	for {
		if added.Load() > 0 {
			break
		}
		select {
		case <-waitCtx.Done():
			punchCancel()
			wg.Wait()
			if added.Load() == 0 {
				return fmt.Errorf("no connections passed auth/session handshake")
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	// Cancel punch loops once at least one connection is live; remaining
	// handshakes will complete with their own punch contexts.
	punchCancel()

	s.cfg.Logger.Info("MUX connections ready",
		"active", m.ActiveConns(),
	)

	// Ensure remaining handshakes finish in the background.
	go func() {
		wg.Wait()
		s.cfg.Logger.Info("all relay handshakes completed", "added", added.Load(), "total", pairCount)
	}()

	// Drain dead connections so their MUX slots are reused by AddConn.
	go func() {
		for {
			select {
			case idx, ok := <-m.ConnDied():
				if !ok {
					return
				}
				m.RemoveConn(idx)
				s.cfg.Logger.Info("removed dead connection from MUX", "index", idx, "active", m.ActiveConns())
			case <-sessCtx.Done():
				return
			}
		}
	}()

	go s.handleReconnections(sessCtx, sigClient, mgr, m)

	go func() {
		reason, disconnectNonce := sigClient.WaitForSessionEnd(sessCtx)
		switch reason {
		case provider.SessionEndDisconnect:
			s.cfg.Logger.Info("client sent disconnect signal, sending ack", "nonce", disconnectNonce)
			_ = sigClient.SendDisconnectAck(sessCtx, disconnectNonce)
			sessCancel()
		case provider.SessionEndHungup:
			s.cfg.Logger.Info("signaling hungup, stopping pings and reducing idle timeout")
			pingCancel()
			m.SetIdleTimeout(15 * time.Second)
			go sigClient.DrainAndRoute(sessCtx)
		}
	}()

	go func() {
		select {
		case <-m.Dead():
			sessCancel()
		case <-sessCtx.Done():
		}
	}()

	for {
		select {
		case stream, ok := <-m.AcceptedStreams():
			if !ok {
				return nil
			}
			go handleStream(sessCtx, s.cfg.Logger, stream)
		case <-m.Dead():
			return nil
		case <-sessCtx.Done():
			return nil
		}
	}
}

func (s *Server) handleReconnections(ctx context.Context, sigClient provider.SignalingClient,
	mgr *turn.Manager, m *mux.Mux) {

	ch, unsub := sigClient.Subscribe(internalsignal.WireConnNew, 8)
	defer unsub()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			payload, err := base64.StdEncoding.DecodeString(msg.Payload)
			if err != nil {
				s.cfg.Logger.Warn("decode reconnect payload", "err", err)
				continue
			}
			clientAddr := string(payload)
			go func() {
				if err := s.handleOneReconnect(ctx, sigClient, mgr, m, clientAddr); err != nil {
					s.cfg.Logger.Warn("reconnect handler failed", "client_addr", clientAddr, "err", err)
				}
			}()
		}
	}
}

func (s *Server) handleOneReconnect(ctx context.Context, sigClient provider.SignalingClient,
	mgr *turn.Manager, m *mux.Mux, clientAddr string) error {

	clientUDP, err := net.ResolveUDPAddr("udp", clientAddr)
	if err != nil {
		return fmt.Errorf("resolve client addr: %w", err)
	}

	allocs, err := mgr.Allocate(ctx, 1)
	if err != nil {
		return fmt.Errorf("allocate TURN: %w", err)
	}
	alloc := allocs[0]
	myAddr := alloc.RelayAddr.String()

	if err := sigClient.SendPayload(ctx, internalsignal.WireConnOk, []byte(myAddr)); err != nil {
		return fmt.Errorf("send conn-ok: %w", err)
	}
	s.cfg.Logger.Info("reconnect: sent relay addr to client", "my_addr", myAddr, "client_addr", clientAddr)

	punchCtx, punchCancel := context.WithCancel(ctx)
	defer punchCancel()
	internaldtls.PunchRelay(alloc.RelayConn, clientUDP)
	go internaldtls.StartPunchLoop(punchCtx, alloc.RelayConn, clientUDP)
	time.Sleep(200 * time.Millisecond)

	reconnCtx, reconnCancel := context.WithTimeout(ctx, 3*time.Second)
	defer reconnCancel()
	dtlsConn, _, err := internaldtls.AcceptOverTURN(reconnCtx, alloc.RelayConn, clientUDP)
	if err != nil {
		return fmt.Errorf("AcceptOverTURN: %w", err)
	}
	punchCancel()

	if s.cfg.AuthToken != "" {
		if err := mux.ValidateAuthToken(dtlsConn, s.cfg.AuthToken); err != nil {
			dtlsConn.Close()
			return fmt.Errorf("auth: %w", err)
		}
	}
	if _, err := mux.ReadSessionID(dtlsConn); err != nil {
		dtlsConn.Close()
		return fmt.Errorf("read session id: %w", err)
	}

	m.AddConn(dtlsConn)
	s.cfg.Logger.Info("reconnect: new connection added to MUX",
		"active", m.ActiveConns(),
		"total", m.TotalConns(),
	)
	return nil
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

	logger.Info("stream opened", "stream_id", stream.ID, "target", target)

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

	// stream → outConn (upload: client sends to target)
	go func() {
		defer wg.Done()
		n, err := io.CopyBuffer(outConn, stream, buf)
		logger.Info("stream→target done", "stream_id", stream.ID, "target", target, "bytes", n, "err", err)
	}()

	// outConn → stream (download: target sends to client)
	go func() {
		defer wg.Done()
		buf2 := make([]byte, mux.MaxFramePayload)
		n, err := io.CopyBuffer(stream, outConn, buf2)
		logger.Info("target→stream done", "stream_id", stream.ID, "target", target, "bytes", n, "err", err)
	}()

	wg.Wait()
	logger.Info("stream relay ended", "stream_id", stream.ID, "target", target)
}
