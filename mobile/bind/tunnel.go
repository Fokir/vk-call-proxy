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
	"os"
	"sync"
	"sync/atomic"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

// Tunnel is the main gomobile-exported type for mobile platforms.
type Tunnel struct {
	mu         sync.Mutex
	mgr        *turn.Manager
	m          *mux.Mux
	logger     *slog.Logger
	cancel     context.CancelFunc
	cleanups   []context.CancelFunc
	nextStream atomic.Uint32
	running    bool
}

// TunnelConfig holds configuration for starting the tunnel.
type TunnelConfig struct {
	CallLink   string // VK call-link ID
	ServerAddr string // VPN server address (host:port)
	NumConns   int    // parallel TURN+DTLS connections
	UseTCP     bool   // TCP vs UDP for TURN
}

// NewTunnel creates a new tunnel instance.
func NewTunnel() *Tunnel {
	return &Tunnel{
		logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// Start establishes TURN+DTLS connections and starts the mux tunnel.
func (t *Tunnel) Start(cfg *TunnelConfig) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.running {
		return fmt.Errorf("tunnel already running")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	serverAddr, err := net.ResolveUDPAddr("udp", cfg.ServerAddr)
	if err != nil {
		cancel()
		return fmt.Errorf("resolve server: %w", err)
	}

	sessionID := uuid.New()

	// 1. Create TURN allocations.
	t.mgr = turn.NewManager(cfg.CallLink, cfg.UseTCP, t.logger)

	allocs, err := t.mgr.Allocate(ctx, cfg.NumConns)
	if err != nil {
		t.mgr.CloseAll()
		cancel()
		return fmt.Errorf("allocate TURN: %w", err)
	}

	// 2. Establish DTLS-over-TURN connections.
	var muxConns []io.ReadWriteCloser
	for i, alloc := range allocs {
		dtlsConn, cleanup, err := internaldtls.DialOverTURN(ctx, alloc.RelayConn, serverAddr)
		if err != nil {
			t.logger.Warn("DTLS-over-TURN failed", "index", i, "err", err)
			continue
		}
		t.cleanups = append(t.cleanups, cleanup)

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
		t.mgr.CloseAll()
		for _, c := range t.cleanups {
			c()
		}
		t.cleanups = nil
		cancel()
		return fmt.Errorf("no DTLS connections established")
	}

	// 3. Create multiplexer.
	t.m = mux.New(t.logger, muxConns...)
	go t.m.DispatchLoop(ctx)
	go t.m.StartPingLoop(ctx, 30*time.Second)

	t.running = true
	t.logger.Info("tunnel started", "connections", len(muxConns), "session_id", sessionID.String())
	return nil
}

// DialStream opens a new mux stream to the given target address (host:port).
func (t *Tunnel) DialStream(addr string) (io.ReadWriteCloser, error) {
	t.mu.Lock()
	m := t.m
	t.mu.Unlock()

	if m == nil {
		return nil, fmt.Errorf("tunnel not running")
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

// WritePacket sends a raw IP packet through the tunnel (for VpnService / NEPacketTunnelProvider).
func (t *Tunnel) WritePacket(data []byte) error {
	t.mu.Lock()
	m := t.m
	t.mu.Unlock()

	if m == nil {
		return fmt.Errorf("tunnel not running")
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
func (t *Tunnel) ReadPacket(buf []byte) (int, error) {
	t.mu.Lock()
	m := t.m
	t.mu.Unlock()

	if m == nil {
		return 0, fmt.Errorf("tunnel not running")
	}

	frame, ok := <-m.RecvFrames()
	if !ok {
		return 0, fmt.Errorf("tunnel closed")
	}
	n := copy(buf, frame.Payload)
	return n, nil
}

// Stop tears down all connections.
func (t *Tunnel) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.running {
		return
	}

	if t.cancel != nil {
		t.cancel()
	}
	if t.m != nil {
		t.m.Close()
	}
	for _, c := range t.cleanups {
		c()
	}
	t.cleanups = nil
	if t.mgr != nil {
		t.mgr.CloseAll()
	}
	t.running = false
	t.logger.Info("tunnel stopped")
}

// IsRunning returns whether the tunnel is active.
func (t *Tunnel) IsRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}
