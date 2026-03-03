// Package bind provides gomobile bindings for the VPN tunnel core.
// Build with: gomobile bind -target android ./mobile/bind/
//             gomobile bind -target ios ./mobile/bind/
package bind

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/mux"
	internalsignal "github.com/call-vpn/call-vpn/internal/signal"
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
	mgr      *turn.Manager
	m        *mux.Mux
	cleanups []context.CancelFunc
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
	rootCtx    context.Context
	rootCancel context.CancelFunc
	muxReady   chan struct{}        // closed when mux is available; recreated on teardown
	muxCancel  context.CancelFunc  // cancels DispatchLoop/PingLoop for current mux
}

// TunnelConfig holds configuration for starting the tunnel.
type TunnelConfig struct {
	CallLink   string // VK call-link ID
	ServerAddr string // VPN server address (host:port), empty = relay-to-relay mode
	NumConns   int    // parallel TURN+DTLS connections
	UseTCP     bool   // TCP vs UDP for TURN
	Token      string // auth token for server (empty = no auth)
}

// NewTunnel creates a new tunnel instance.
func NewTunnel() *Tunnel {
	lb := NewLogBuffer(50)
	return &Tunnel{
		logBuf: lb,
		logger: slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelInfo})),
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

// Start establishes TURN+DTLS connections and starts the mux tunnel.
// If ServerAddr is empty, uses relay-to-relay mode via VK signaling.
// A background reconnect loop monitors connection health and re-establishes
// the tunnel automatically when all DTLS connections die.
func (t *Tunnel) Start(cfg *TunnelConfig) error {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return fmt.Errorf("tunnel already running")
	}
	t.cfg = cfg
	t.rootCtx, t.rootCancel = context.WithCancel(context.Background())
	t.muxReady = make(chan struct{})
	t.running = true
	t.mu.Unlock()

	var state *tunnelState
	var err error
	if cfg.ServerAddr != "" {
		state, err = t.connectDirect(t.rootCtx, cfg)
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

// connectDirect creates TURN allocations and DTLS connections to a server
// listening on a direct UDP address. Returns a tunnelState without mutating t.
func (t *Tunnel) connectDirect(ctx context.Context, cfg *TunnelConfig) (*tunnelState, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", cfg.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve server: %w", err)
	}

	sessionID := uuid.New()

	mgr := turn.NewManager(cfg.CallLink, cfg.UseTCP, t.logger)
	allocs, err := mgr.Allocate(ctx, cfg.NumConns)
	if err != nil {
		mgr.CloseAll()
		return nil, fmt.Errorf("allocate TURN: %w", err)
	}

	var muxConns []io.ReadWriteCloser
	var cleanups []context.CancelFunc
	for i, alloc := range allocs {
		dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, alloc.RelayConn, serverAddr)
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

	m := mux.New(t.logger, muxConns...)
	t.logger.Info("tunnel connected (direct)", "connections", len(muxConns), "session_id", sessionID.String())
	return &tunnelState{mgr: mgr, m: m, cleanups: cleanups}, nil
}

// connectRelay creates TURN allocations, exchanges relay addresses via VK
// signaling, and establishes DTLS connections to the server's relay addresses.
// Returns a tunnelState without mutating t.
func (t *Tunnel) connectRelay(ctx context.Context, cfg *TunnelConfig) (*tunnelState, error) {
	jr, err := turn.FetchJoinResponse(ctx, cfg.CallLink)
	if err != nil {
		return nil, fmt.Errorf("join VK conference: %w", err)
	}
	t.logger.Info("joined VK conference", "conv_id", jr.ConvID)

	sigClient, err := internalsignal.Connect(ctx, jr.WSEndpoint, t.logger.With("component", "signaling"))
	if err != nil {
		return nil, fmt.Errorf("signaling connect: %w", err)
	}
	if err := sigClient.SetKey(cfg.Token); err != nil {
		sigClient.Close()
		return nil, fmt.Errorf("set signaling key: %w", err)
	}

	mgr := turn.NewManager(cfg.CallLink, cfg.UseTCP, t.logger)
	allocs, err := mgr.Allocate(ctx, cfg.NumConns)
	if err != nil {
		sigClient.Close()
		mgr.CloseAll()
		return nil, fmt.Errorf("allocate TURN: %w", err)
	}

	ourAddrs := make([]string, len(allocs))
	for i, a := range allocs {
		ourAddrs[i] = a.RelayAddr.String()
	}

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

	serverAddrs, _, err := sigClient.RecvRelayAddrs(ctx)
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
		sigClient.Close()
	}()

	pairCount := len(allocs)
	if len(serverAddrs) < pairCount {
		pairCount = len(serverAddrs)
	}

	for i := 0; i < pairCount; i++ {
		serverUDP, err := net.ResolveUDPAddr("udp", serverAddrs[i])
		if err != nil {
			continue
		}
		internaldtls.PunchRelay(allocs[i].RelayConn, serverUDP)
	}
	time.Sleep(500 * time.Millisecond)

	sessionID := uuid.New()

	var muxConns []io.ReadWriteCloser
	var cleanups []context.CancelFunc
	for i := 0; i < pairCount; i++ {
		serverUDP, err := net.ResolveUDPAddr("udp", serverAddrs[i])
		if err != nil {
			continue
		}

		dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, allocs[i].RelayConn, serverUDP)
		if err != nil {
			t.logger.Warn("relay DTLS failed", "index", i, "err", err)
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
		return nil, fmt.Errorf("no relay DTLS connections established")
	}

	m := mux.New(t.logger, muxConns...)
	t.logger.Info("tunnel connected (relay-to-relay)", "connections", len(muxConns), "session_id", sessionID.String())
	return &tunnelState{mgr: mgr, m: m, cleanups: cleanups}, nil
}

// applyState installs a new tunnelState into the tunnel, starting
// DispatchLoop and PingLoop, and signals muxReady.
// Raw packet mode is enabled so DispatchLoop routes StreamID=0 FrameData
// to a dedicated channel consumed by ReadPacket.
func (t *Tunnel) applyState(state *tunnelState) {
	t.mu.Lock()
	t.m = state.m
	t.mgr = state.mgr
	t.cleanups = state.cleanups
	muxCtx, muxCancel := context.WithCancel(t.rootCtx)
	t.muxCancel = muxCancel
	ready := t.muxReady
	t.mu.Unlock()

	state.m.EnableRawPackets(256)
	go state.m.DispatchLoop(muxCtx)
	go state.m.StartPingLoop(muxCtx, 30*time.Second)

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

	t.muxCancel = nil
	t.m = nil
	t.cleanups = nil
	t.mgr = nil
	t.muxReady = make(chan struct{})
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
}

// reconnectLoop watches the current mux for death and re-establishes
// the tunnel with exponential backoff (1s → 60s).
func (t *Tunnel) reconnectLoop() {
	const maxBackoff = 60 * time.Second
	const attemptTimeout = 30 * time.Second
	backoff := time.Second

	for {
		t.mu.Lock()
		m := t.m
		t.mu.Unlock()

		if m == nil {
			select {
			case <-t.rootCtx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}

		select {
		case <-m.Dead():
			t.logger.Info("all connections dead, starting reconnect")
		case <-t.rootCtx.Done():
			return
		}

		t.teardownMux()

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
			} else {
				state, err = t.connectRelay(attemptCtx, t.cfg)
			}
			attemptCancel()

			if err != nil {
				t.logger.Warn("reconnect attempt failed", "err", err, "backoff", backoff)
				backoff = min(backoff*2, maxBackoff)
				continue
			}

			t.applyState(state)
			t.logger.Info("reconnected successfully")
			backoff = time.Second
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
// Blocks during reconnect until the mux is available.
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
// Keeps raw IP packets + MUX header + DTLS overhead within TURN relay limits.
const TunMTU = 1280

// WritePacket sends a raw IP packet through the tunnel (for VpnService / NEPacketTunnelProvider).
// Returns error immediately during reconnect (packets are dropped; native code retries).
// Packets larger than TunMTU are silently dropped (like an oversized datagram on a real interface).
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

	return m.SendFrame(&mux.Frame{
		StreamID: 0,
		Type:     mux.FrameData,
		Sequence: m.NextSeq(),
		Length:   uint32(len(data)),
		Payload:  data,
	})
}

// ReadPacket reads a raw IP packet from the tunnel.
// Blocks during reconnect until the mux is re-established.
// Only receives FrameData with StreamID=0 (raw IP packets) via the
// dedicated RawPackets channel, avoiding contention with DispatchLoop.
func (t *Tunnel) ReadPacket(buf []byte) (int, error) {
	for {
		m, err := t.getMux()
		if err != nil {
			return 0, err
		}

		ch := m.RawPackets()
		if ch == nil {
			return 0, fmt.Errorf("raw packet mode not enabled")
		}

		select {
		case frame, ok := <-ch:
			if !ok {
				// Channel closed — mux died. Wait briefly for reconnect loop
				// to call teardownMux (sets t.m=nil), then loop back —
				// getMux will block on the new muxReady channel.
				select {
				case <-t.rootCtx.Done():
					return 0, fmt.Errorf("tunnel stopped")
				case <-time.After(100 * time.Millisecond):
					continue
				}
			}
			if len(frame.Payload) == 0 {
				continue
			}
			n := copy(buf, frame.Payload)
			return n, nil
		case <-t.rootCtx.Done():
			return 0, fmt.Errorf("tunnel stopped")
		}
	}
}

// IsConnected reports whether the tunnel currently has an active mux.
// Returns false during reconnection.
func (t *Tunnel) IsConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m != nil
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
	t.mu.Unlock()

	if rootCancel != nil {
		rootCancel()
	}
	t.teardownMux()
	t.logger.Info("tunnel stopped")
}

// IsRunning returns whether the tunnel is active.
func (t *Tunnel) IsRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}
