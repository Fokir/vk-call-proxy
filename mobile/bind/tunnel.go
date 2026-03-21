// Package bind provides gomobile bindings for the VPN tunnel core.
// Build with: gomobile bind -target android ./mobile/bind/
//             gomobile bind -target ios ./mobile/bind/
package bind

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/call-vpn/call-vpn/internal/client"
	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/telemost"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

// LogBuffer is a thread-safe ring buffer that implements io.Writer.
// It stores the last N log lines for retrieval by native mobile code.
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	cap   int
}

// NewLogBuffer creates a ring buffer with the given capacity.
func NewLogBuffer(capacity int) *LogBuffer {
	return &LogBuffer{
		lines: make([]string, 0, capacity),
		cap:   capacity,
	}
}

// Write implements io.Writer. It splits input by newlines and appends each line.
func (lb *LogBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	text := string(p)
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if line == "" {
			continue
		}
		if len(lb.lines) >= lb.cap {
			lb.lines = lb.lines[1:]
		}
		lb.lines = append(lb.lines, line)
	}
	return len(p), nil
}

// ReadAll returns all buffered lines joined by newlines.
func (lb *LogBuffer) ReadAll() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return strings.Join(lb.lines, "\n")
}

// Clear removes all buffered lines.
func (lb *LogBuffer) Clear() {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.lines = lb.lines[:0]
}

// ReadAndClear atomically returns all buffered lines and clears the buffer.
func (lb *LogBuffer) ReadAndClear() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	result := strings.Join(lb.lines, "\n")
	lb.lines = lb.lines[:0]
	return result
}

// tunnelState holds the result of a connection attempt.
// Returned by connectDirect/connectRelay for applyState to install.
type tunnelState struct {
	mgr       *turn.Manager
	m         *mux.Mux
	conns     []io.ReadWriteCloser       // connections to add after idle timeout is set
	cleanups  []context.CancelFunc
	sigClient provider.SignalingClient    // non-nil in relay mode; kept alive for reconnect signaling
	sessionID uuid.UUID                  // session UUID for reconnect auth
}

// Tunnel is the main gomobile-exported type for mobile platforms.
type Tunnel struct {
	mu         sync.Mutex
	mgr        *turn.Manager
	m          *mux.Mux
	logger     *slog.Logger
	logBuf     *LogBuffer
	cleanups   []context.CancelFunc
	nextStream atomic.Uint32
	running    bool

	// reconnect infrastructure
	cfg        *TunnelConfig
	fpBytes    []byte          // parsed fingerprint bytes from cfg.Fingerprint
	svc        provider.Service
	rootCtx    context.Context
	rootCancel context.CancelFunc
	muxReady   chan struct{}           // closed when mux is available; recreated on teardown
	muxCancel  context.CancelFunc     // cancels DispatchLoop/PingLoop for current mux
	connectedAt time.Time             // when the current MUX was established
	sigClient  provider.SignalingClient // kept alive in relay mode for per-conn reconnects
	sessionID  uuid.UUID              // current session ID

	// network change debouncing
	networkDebounce *time.Timer    // coalesces rapid network change events
	networkForce    chan struct{}   // signals reconnectLoop to do immediate full reconnect

	// connection stage (for UI display)
	stage atomic.Value // string
}

// MaxRecommendedConns is the maximum number of parallel connections
// recommended for stable operation. Exceeding this may cause VK call
// instability and potential call blocking.
const MaxRecommendedConns = 8

// TunnelConfig holds configuration for starting the tunnel.
type TunnelConfig struct {
	CallLink    string // call-link ID
	ServerAddr  string // VPN server address (host:port), empty = relay-to-relay mode
	NumConns    int    // parallel TURN+DTLS connections
	UseTCP      bool   // TCP vs UDP for TURN
	Token       string // auth token for server (empty = no auth)
	Fingerprint string // server DTLS certificate SHA-256 fingerprint (hex, empty = no pinning)
}

// ValidateNumConns returns a warning message if NumConns exceeds the
// recommended maximum, or an empty string if the value is safe.
// Mobile apps should call this and display the warning under the input field.
func ValidateNumConns(n int) string {
	if n > MaxRecommendedConns {
		return fmt.Sprintf("Warning: %d connections exceeds the recommended maximum of %d. "+
			"This may cause call instability and potential VK call blocking.", n, MaxRecommendedConns)
	}
	return ""
}

// NewTunnel creates a new tunnel instance.
func NewTunnel() *Tunnel {
	lb := NewLogBuffer(500)
	return &Tunnel{
		logBuf: lb,
		logger: slog.New(slog.NewTextHandler(io.MultiWriter(lb, os.Stderr), &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// ReadLogs returns all buffered log lines as a single string
// and atomically clears the buffer.
func (t *Tunnel) ReadLogs() string {
	return t.logBuf.ReadAndClear()
}

// ClearLogs removes all buffered log lines.
func (t *Tunnel) ClearLogs() {
	t.logBuf.Clear()
}

// Stage returns the current connection stage for UI display.
// Returns empty string when connected or idle.
func (t *Tunnel) Stage() string {
	v := t.stage.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func (t *Tunnel) setStage(s string) {
	t.stage.Store(s)
	t.logger.Info("stage: " + s)
}

// Start establishes TURN+DTLS connections and starts the mux tunnel.
// If ServerAddr is empty, uses relay-to-relay mode via signaling.
// A background reconnect loop monitors connection health and re-establishes
// the tunnel automatically when all DTLS connections die.
func (t *Tunnel) Start(cfg *TunnelConfig) error {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return fmt.Errorf("tunnel already running")
	}
	if cfg.NumConns <= 0 {
		cfg.NumConns = 16
	}
	if warn := ValidateNumConns(cfg.NumConns); warn != "" {
		t.logger.Warn(warn)
	}
	t.cfg = cfg
	if cfg.Fingerprint != "" {
		fp, err := hex.DecodeString(cfg.Fingerprint)
		if err != nil || len(fp) != 32 {
			t.mu.Unlock()
			return fmt.Errorf("invalid fingerprint: must be 64 hex characters (SHA-256)")
		}
		t.fpBytes = fp
	} else {
		t.fpBytes = nil
	}
	if telemost.IsTelemostLink(cfg.CallLink) {
		t.svc = telemost.NewService(cfg.CallLink, cfg.Token)
	} else {
		t.svc = vk.NewService(cfg.CallLink)
	}
	t.rootCtx, t.rootCancel = context.WithCancel(context.Background())
	t.muxReady = make(chan struct{})
	t.networkForce = make(chan struct{}, 1)
	t.running = true
	t.mu.Unlock()

	var state *tunnelState
	var err error
	if cfg.ServerAddr != "" {
		state, err = t.connectDirect(t.rootCtx, cfg)
	} else if telemost.IsTelemostLink(cfg.CallLink) {
		state, err = t.connectTelemost(t.rootCtx, cfg)
	} else {
		state, err = t.connectRelay(t.rootCtx, cfg)
	}
	if err != nil {
		t.rootCancel()
		t.mu.Lock()
		t.running = false
		t.mu.Unlock()
		return err
	}

	t.applyState(state)
	go t.reconnectLoop()
	return nil
}

// connectTelemost establishes Telemost WebRTC connections using paired display
// names derived from the auth token. Returns a tunnelState without mutating t.
func (t *Tunnel) connectTelemost(ctx context.Context, cfg *TunnelConfig) (*tunnelState, error) {
	t.mu.Lock()
	svc := t.svc
	t.mu.Unlock()

	tmSvc, ok := svc.(*telemost.Service)
	if !ok {
		return nil, fmt.Errorf("expected telemost.Service")
	}

	serverNames, clientNames := telemost.DeriveDisplayNames(cfg.Token, cfg.NumConns)
	sessionID := uuid.New()

	var muxConns []io.ReadWriteCloser
	var cleanups []context.CancelFunc
	for i := 0; i < cfg.NumConns; i++ {
		conn, cleanup, err := tmSvc.ConnectPaired(ctx,
			t.logger.With("index", i), clientNames[i], serverNames[i], i)
		if err != nil {
			t.logger.Warn("Telemost connection failed", "index", i, "err", err)
			continue
		}

		if cfg.Token != "" {
			if err := mux.WriteAuthToken(conn, cfg.Token); err != nil {
				t.logger.Warn("write auth token failed", "index", i, "err", err)
				cleanup()
				continue
			}
		}

		var sid [16]byte
		copy(sid[:], sessionID[:])
		if err := mux.WriteSessionID(conn, sid); err != nil {
			t.logger.Warn("write session id failed", "index", i, "err", err)
			cleanup()
			continue
		}

		cleanups = append(cleanups, cleanup)
		muxConns = append(muxConns, conn)
	}

	if len(muxConns) == 0 {
		for _, c := range cleanups {
			c()
		}
		return nil, fmt.Errorf("no Telemost connections established")
	}

	m := mux.New(t.logger)
	t.logger.Info("tunnel connected (telemost)",
		"active", len(muxConns), "target", cfg.NumConns,
		"session_id", sessionID.String())
	return &tunnelState{m: m, conns: muxConns, cleanups: cleanups, sessionID: sessionID}, nil
}

// connectDirect creates TURN allocations and DTLS connections to a server
// listening on a direct UDP address. Returns a tunnelState without mutating t.
func (t *Tunnel) connectDirect(ctx context.Context, cfg *TunnelConfig) (*tunnelState, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", cfg.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve server: %w", err)
	}

	sessionID := uuid.New()

	t.mu.Lock()
	svc := t.svc
	t.mu.Unlock()

	mgr := turn.NewManager(svc, cfg.UseTCP, t.logger)
	allocs, err := mgr.Allocate(ctx, cfg.NumConns)
	if err != nil {
		mgr.CloseAll()
		return nil, fmt.Errorf("allocate TURN: %w", err)
	}

	var muxConns []io.ReadWriteCloser
	var cleanups []context.CancelFunc
	for i, alloc := range allocs {
		dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, alloc.RelayConn, serverAddr, t.fpBytes)
		if err != nil {
			t.logger.Warn("DTLS-over-TURN failed", "index", i, "err", err)
			continue
		}
		cleanups = append(cleanups, cleanup)

		if cfg.Token != "" {
			if err := mux.WriteAuthToken(dtlsConn, cfg.Token); err != nil {
				t.logger.Warn("write auth token failed", "index", i, "err", err)
				cleanup()
				continue
			}
		}

		var sid [16]byte
		copy(sid[:], sessionID[:])
		if err := mux.WriteSessionID(dtlsConn, sid); err != nil {
			t.logger.Warn("write session id failed", "index", i, "err", err)
			cleanup()
			continue
		}

		muxConns = append(muxConns, dtlsConn)
	}

	if len(muxConns) == 0 {
		mgr.CloseAll()
		for _, c := range cleanups {
			c()
		}
		return nil, fmt.Errorf("no DTLS connections established")
	}

	m := mux.New(t.logger)
	t.logger.Info("tunnel connected (direct)",
		"active", len(muxConns), "target", cfg.NumConns,
		"session_id", sessionID.String())
	return &tunnelState{mgr: mgr, m: m, conns: muxConns, cleanups: cleanups}, nil
}

// connectRelay creates TURN allocations, exchanges relay addresses via
// signaling, and establishes DTLS connections to the server's relay addresses.
// Returns a tunnelState without mutating t.
func (t *Tunnel) connectRelay(ctx context.Context, cfg *TunnelConfig) (*tunnelState, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("token is required for relay-to-relay mode")
	}

	t.mu.Lock()
	svc := t.svc
	t.mu.Unlock()

	t.setStage("Получение токена VK...")
	jr, err := svc.FetchJoinInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("join conference: %w", err)
	}
	t.logger.Info("joined conference", "conv_id", jr.ConvID)

	t.setStage("Подключение к TURN...")
	// Start TURN allocations in background — independent of signaling setup.
	type allocResult struct {
		allocs []*turn.Allocation
		mgr    *turn.Manager
		err    error
	}
	allocCh := make(chan allocResult, 1)
	go func() {
		mgr := turn.NewManager(svc, cfg.UseTCP, t.logger)
		allocs, err := mgr.Allocate(ctx, cfg.NumConns)
		allocCh <- allocResult{allocs, mgr, err}
	}()

	// Meanwhile, set up signaling (connect + SetKey + disconnect handshake).
	t.setStage("Подключение к сигналингу...")
	sigClient, err := svc.ConnectSignaling(ctx, jr, t.logger.With("component", "signaling"))
	if err != nil {
		ar := <-allocCh
		if ar.mgr != nil {
			ar.mgr.CloseAll()
		}
		return nil, fmt.Errorf("signaling connect: %w", err)
	}
	if err := sigClient.SetKey(cfg.Token); err != nil {
		sigClient.Close()
		ar := <-allocCh
		if ar.mgr != nil {
			ar.mgr.CloseAll()
		}
		return nil, fmt.Errorf("set signaling key: %w", err)
	}

	// Generate session nonce for filtering ghost messages.
	nonceBytes := make([]byte, 8)
	rand.Read(nonceBytes)
	nonce := hex.EncodeToString(nonceBytes)

	t.setStage("Сброс старой сессии...")
	// Tell server to kill any existing session (disconnect-req + ack handshake).
	const disconnectRetries = 3
	for i := 0; i < disconnectRetries; i++ {
		_ = sigClient.SendDisconnectReq(ctx, nonce)
		ackCtx, ackCancel := context.WithTimeout(ctx, 2*time.Second)
		err := sigClient.WaitDisconnectAck(ackCtx, nonce)
		ackCancel()
		if err == nil {
			t.logger.Info("disconnect ack received", "nonce", nonce)
			break
		}
		t.logger.Debug("disconnect ack timeout, retrying", "attempt", i+1, "nonce", nonce)
	}

	t.setStage("Ожидание TURN...")
	// Wait for TURN allocations to complete.
	ar := <-allocCh
	if ar.err != nil {
		sigClient.Close()
		if ar.mgr != nil {
			ar.mgr.CloseAll()
		}
		return nil, fmt.Errorf("allocate TURN: %w", ar.err)
	}
	allocs := ar.allocs
	mgr := ar.mgr

	ourAddrs := make([]string, len(allocs))
	for i, a := range allocs {
		ourAddrs[i] = a.RelayAddr.String()
	}

	t.setStage("Обмен relay адресами...")
	sendDone := make(chan struct{})
	sendCtx, sendCancel := context.WithCancel(ctx)
	go func() {
		defer close(sendDone)
		for {
			if err := sigClient.SendRelayAddrs(sendCtx, ourAddrs, "client", nonce); err != nil {
				return
			}
			select {
			case <-sendCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()

	serverAddrs, _, _, err := sigClient.RecvRelayAddrs(ctx, "client", nonce)
	if err != nil {
		sendCancel()
		<-sendDone
		sigClient.Close()
		mgr.CloseAll()
		return nil, fmt.Errorf("recv relay addrs: %w", err)
	}

	go func() {
		time.Sleep(5 * time.Second)
		sendCancel()
		<-sendDone
	}()

	pairCount := len(allocs)
	if len(serverAddrs) < pairCount {
		pairCount = len(serverAddrs)
	}

	sessionID := uuid.New()

	type dtlsResult struct {
		index   int
		conn    io.ReadWriteCloser
		cleanup context.CancelFunc
		err     error
	}
	results := make(chan dtlsResult, pairCount)
	punchCtx, punchCancel := context.WithCancel(ctx)

	t.setStage("Установка DTLS...")
	sigClient.StartPunchDispatcher(ctx, nonce)
	defer sigClient.StopPunchDispatcher()

	for i := 0; i < pairCount; i++ {
		serverUDP, err := net.ResolveUDPAddr("udp", serverAddrs[i])
		if err != nil {
			results <- dtlsResult{index: i, err: err}
			continue
		}
		go func(idx int, relayConn net.PacketConn, addr *net.UDPAddr) {
			var dtlsConn io.ReadWriteCloser
			var cleanup context.CancelFunc
			var lastErr error
			for attempt := 1; attempt <= 2; attempt++ {
				punchLoopCtx, punchLoopCancel := context.WithCancel(punchCtx)
				internaldtls.PunchRelay(relayConn, addr)
				go internaldtls.StartPunchLoop(punchLoopCtx, relayConn, addr)

				punchReadyCtx, prc := context.WithTimeout(ctx, 3*time.Second)
				waitPunch := sigClient.PreparePunchWait(punchReadyCtx, nonce, idx)
				_ = sigClient.SendPunchReady(ctx, nonce, idx)
				_ = waitPunch()
				prc()

				var c net.Conn
				c, cleanup, lastErr = internaldtls.DialOverTURN(ctx, relayConn, addr, t.fpBytes)
				punchLoopCancel()

				if lastErr == nil {
					dtlsConn = c
					break
				}
				t.logger.Warn("DTLS handshake failed", "attempt", attempt, "index", idx, "err", lastErr)
				if attempt < 2 {
					time.Sleep(time.Duration(attempt) * time.Second)
				}
			}
			if lastErr != nil {
				results <- dtlsResult{index: idx, err: lastErr}
				return
			}

			if cfg.Token != "" {
				if err := mux.WriteAuthToken(dtlsConn, cfg.Token); err != nil {
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

	var m *mux.Mux
	var muxConns []io.ReadWriteCloser
	var cleanups []context.CancelFunc
	for j := 0; j < pairCount; j++ {
		r := <-results
		if r.err != nil {
			t.logger.Warn("relay DTLS failed", "index", r.index, "err", r.err)
			continue
		}
		cleanups = append(cleanups, r.cleanup)
		if m == nil {
			// First successful connection — create MUX immediately.
			m = mux.New(t.logger)
			t.logger.Info("first relay connection ready, MUX created", "index", r.index)
		}
		muxConns = append(muxConns, r.conn)
	}
	punchCancel()

	if m == nil {
		mgr.CloseAll()
		for _, c := range cleanups {
			c()
		}
		return nil, fmt.Errorf("no relay DTLS connections established")
	}

	t.logger.Info("tunnel connected (relay-to-relay)",
		"active", len(muxConns), "target", cfg.NumConns,
		"session_id", sessionID.String())
	return &tunnelState{mgr: mgr, m: m, conns: muxConns, cleanups: cleanups, sigClient: sigClient, sessionID: sessionID}, nil
}

// applyState installs a new tunnelState into the tunnel, starting
// DispatchLoop and PingLoop, and signals muxReady.
func (t *Tunnel) applyState(state *tunnelState) {
	t.mu.Lock()
	t.m = state.m
	t.mgr = state.mgr
	t.cleanups = state.cleanups
	t.sigClient = state.sigClient
	t.sessionID = state.sessionID
	t.connectedAt = time.Now()
	muxCtx, muxCancel := context.WithCancel(t.rootCtx)
	t.muxCancel = muxCancel
	ready := t.muxReady
	t.mu.Unlock()

	t.setStage("") // clear stage — connected
	state.m.EnableRawPackets(4096)
	state.m.SetIdleTimeout(15 * time.Second)
	go state.m.DispatchLoop(muxCtx)
	go state.m.StartPingLoop(muxCtx, 5*time.Second)
	if state.mgr != nil {
		go state.mgr.StartKeepalive(muxCtx, 10*time.Second)
	}

	// Add connections AFTER idle timeout and DispatchLoop are set up.
	for _, conn := range state.conns {
		state.m.AddConn(conn)
	}

	// Start per-connection reconnect for relay mode.
	if state.sigClient != nil {
		go client.NewReconnectManager(client.ReconnectConfig{
			TargetConns:     t.cfg.NumConns,
			AuthToken:       t.cfg.Token,
			Fingerprint:     t.fpBytes,
			SessionID:       state.sessionID,
			Logger:          t.logger,
			OnFullReconnect: func() { t.teardownMux() },
		}, state.sigClient, state.mgr, state.m).Run(muxCtx)
		go func() {
			reason, _ := state.sigClient.WaitForSessionEnd(muxCtx)
			if reason == provider.SessionEndHungup {
				t.logger.Warn("VK terminated the call (hungup), triggering full session reconnect")
				t.teardownMux()
			}
		}()
	}

	close(ready)
}

// teardownMux idempotently tears down the current mux, TURN manager, and
// all DTLS cleanups. A new muxReady channel is created for the next connection.
func (t *Tunnel) teardownMux() {
	t.mu.Lock()
	muxCancel := t.muxCancel
	m := t.m
	cleanups := t.cleanups
	mgr := t.mgr
	sig := t.sigClient

	t.muxCancel = nil
	t.m = nil
	t.cleanups = nil
	t.mgr = nil
	t.sigClient = nil
	// Only create a new muxReady if there was something to tear down.
	// Repeated calls with nothing to tear down must not replace the channel
	// (getMux may already be waiting on it).
	if m != nil || muxCancel != nil {
		t.muxReady = make(chan struct{})
	}
	t.mu.Unlock()

	if muxCancel != nil {
		muxCancel()
	}
	if m != nil {
		m.Close()
	}
	for _, c := range cleanups {
		c()
	}
	if mgr != nil {
		mgr.CloseAll()
	}
	if sig != nil {
		sig.Close()
	}
}

// reconnectLoop watches the current mux for death (or network change signal)
// and re-establishes the tunnel with exponential backoff (1s → 60s).
func (t *Tunnel) reconnectLoop() {
	const maxBackoff = 60 * time.Second
	const attemptTimeout = 20 * time.Second
	backoff := time.Second

	for {
		t.mu.Lock()
		m := t.m
		force := t.networkForce
		t.mu.Unlock()

		if m == nil {
			select {
			case <-t.rootCtx.Done():
				return
			case <-force:
			case <-time.After(backoff):
			}
		} else {
			select {
			case <-m.Dead():
				t.logger.Info("all connections dead, starting reconnect")
				t.setStage("Переподключение...")
				t.teardownMux()
			case <-force:
				t.logger.Info("network change forced reconnect")
				t.setStage("Переподключение...")
				t.teardownMux()
			case <-t.rootCtx.Done():
				return
			}
		}

		// Drain any pending force signal so it doesn't trigger again.
		select {
		case <-force:
		default:
		}

		for {
			select {
			case <-t.rootCtx.Done():
				return
			case <-time.After(backoff):
			}

			attemptCtx, attemptCancel := context.WithTimeout(t.rootCtx, attemptTimeout)
			var state *tunnelState
			var err error
			if t.cfg.ServerAddr != "" {
				state, err = t.connectDirect(attemptCtx, t.cfg)
			} else if telemost.IsTelemostLink(t.cfg.CallLink) {
				state, err = t.connectTelemost(attemptCtx, t.cfg)
			} else {
				state, err = t.connectRelay(attemptCtx, t.cfg)
			}
			attemptCancel()

			if err != nil {
				t.logger.Warn("reconnect attempt failed", "err", err, "backoff", backoff)
				backoff = min(backoff*2, maxBackoff)
				select {
				case <-force:
					t.logger.Info("network changed during reconnect, resetting backoff")
					backoff = time.Second
				default:
				}
				continue
			}

			t.applyState(state)
			t.logger.Info("reconnected successfully")
			backoff = time.Second
			select {
			case <-force:
			default:
			}
			break
		}
	}
}

// getMux returns the current mux, blocking until one is available
// if a reconnect is in progress.
func (t *Tunnel) getMux() (*mux.Mux, error) {
	t.mu.Lock()
	m := t.m
	ready := t.muxReady
	t.mu.Unlock()

	if m != nil {
		return m, nil
	}

	select {
	case <-ready:
	case <-t.rootCtx.Done():
		return nil, fmt.Errorf("tunnel stopped")
	}

	t.mu.Lock()
	m = t.m
	t.mu.Unlock()
	if m == nil {
		return nil, fmt.Errorf("tunnel not available")
	}
	return m, nil
}

// DialStream opens a new mux stream to the given target address (host:port).
func (t *Tunnel) DialStream(addr string) (io.ReadWriteCloser, error) {
	m, err := t.getMux()
	if err != nil {
		return nil, err
	}

	id := t.nextStream.Add(1)
	stream, err := m.OpenStream(id)
	if err != nil {
		return nil, err
	}
	if _, err := stream.Write([]byte(addr)); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// TunMTU is the recommended MTU for the native TUN device.
const TunMTU = 1280

// WritePacket sends a raw IP packet through the tunnel.
func (t *Tunnel) WritePacket(data []byte) error {
	t.mu.Lock()
	m := t.m
	t.mu.Unlock()

	if m == nil {
		return fmt.Errorf("tunnel reconnecting")
	}

	if len(data) > TunMTU {
		return nil // drop oversized packet
	}

	return m.SendRawPacket(&mux.Frame{
		StreamID: 0,
		Type:     mux.FrameData,
		Sequence: m.NextSeq(),
		Length:   uint32(len(data)),
		Payload:  data,
	})
}

// ReadPacket reads a raw IP packet from the tunnel.
func (t *Tunnel) ReadPacket(buf []byte) (int, error) {
	for {
		m, err := t.getMux()
		if err != nil {
			return 0, err
		}

		rb := m.RawPackets()
		if rb == nil {
			return 0, fmt.Errorf("raw packet mode not enabled")
		}

		// Try to pop a frame without blocking first.
		if f, ok := rb.Pop(); ok {
			if len(f.Payload) == 0 {
				continue
			}
			n := copy(buf, f.Payload)
			return n, nil
		}

		// Wait for data or shutdown.
		select {
		case _, ok := <-rb.Ready():
			if !ok {
				// Ring buffer closed (mux died). Retry with new mux.
				select {
				case <-t.rootCtx.Done():
					return 0, fmt.Errorf("tunnel stopped")
				case <-time.After(100 * time.Millisecond):
					continue
				}
			}
			// Signaled — pop again.
			if f, ok := rb.Pop(); ok {
				if len(f.Payload) == 0 {
					continue
				}
				n := copy(buf, f.Payload)
				return n, nil
			}
		case <-t.rootCtx.Done():
			return 0, fmt.Errorf("tunnel stopped")
		}
	}
}

// ReadPacketData reads a raw IP packet and returns it as a new byte slice.
func (t *Tunnel) ReadPacketData() ([]byte, error) {
	buf := make([]byte, TunMTU)
	n, err := t.ReadPacket(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// IsConnected reports whether the tunnel currently has an active mux.
func (t *Tunnel) IsConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m != nil
}

// ActiveConns returns the number of active DTLS connections in the mux.
func (t *Tunnel) ActiveConns() int {
	t.mu.Lock()
	m := t.m
	t.mu.Unlock()
	if m == nil {
		return 0
	}
	return m.ActiveConns()
}

// TotalConns returns the target number of connections (from config).
func (t *Tunnel) TotalConns() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cfg == nil {
		return 0
	}
	return int(t.cfg.NumConns)
}

// Stop tears down all connections and stops the reconnect loop.
func (t *Tunnel) Stop() {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	t.running = false
	rootCancel := t.rootCancel
	if t.networkDebounce != nil {
		t.networkDebounce.Stop()
		t.networkDebounce = nil
	}
	t.mu.Unlock()

	if rootCancel != nil {
		rootCancel()
	}
	t.teardownMux()
	t.logger.Info("tunnel stopped")
}

// OnNetworkChanged should be called by the mobile platform when the network
// connectivity changes (e.g. WiFi→cellular).
func (t *Tunnel) OnNetworkChanged() {
	const gracePeriod = 10 * time.Second // WiFi DHCP/IPv6 SLAAC settling period

	t.mu.Lock()
	running := t.running
	connAge := time.Since(t.connectedAt)

	if !running {
		t.mu.Unlock()
		return
	}

	if connAge < gracePeriod {
		t.mu.Unlock()
		t.logger.Info("ignoring network change during grace period", "conn_age", connAge)
		return
	}

	if t.networkDebounce != nil {
		t.networkDebounce.Stop()
		t.networkDebounce = nil
	}

	m := t.m
	force := t.networkForce

	// Drain stale packets before teardown.
	if m != nil {
		if rb := m.RawPackets(); rb != nil {
			if n := rb.Drain(); n > 0 {
				t.logger.Info("drained stale raw packets on network change", "count", n)
			}
		}
	}
	t.mu.Unlock()

	// Always tear down — even if reconnect is in progress, the underlying
	// TCP/TURN connections are bound to the old IP and will timeout slowly.
	// teardownMux is idempotent and only creates a new muxReady when there's
	// something to tear down.
	t.logger.Info("network change detected, tearing down", "had_mux", m != nil)
	t.teardownMux()

	// Signal reconnect loop (non-blocking).
	select {
	case force <- struct{}{}:
	default:
	}
}

// IsRunning returns whether the tunnel is active.
func (t *Tunnel) IsRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}
