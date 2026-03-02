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
	internalsignal "github.com/call-vpn/call-vpn/internal/signal"
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
	ServerAddr string // VPN server address (host:port), empty = relay-to-relay mode
	NumConns   int    // parallel TURN+DTLS connections
	UseTCP     bool   // TCP vs UDP for TURN
	Token      string // auth token for server (empty = no auth)
}

// NewTunnel creates a new tunnel instance.
func NewTunnel() *Tunnel {
	return &Tunnel{
		logger: slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
}

// Start establishes TURN+DTLS connections and starts the mux tunnel.
// If ServerAddr is empty, uses relay-to-relay mode via VK signaling.
func (t *Tunnel) Start(cfg *TunnelConfig) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.running {
		return fmt.Errorf("tunnel already running")
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	var err error
	if cfg.ServerAddr != "" {
		err = t.startDirect(ctx, cfg)
	} else {
		err = t.startRelay(ctx, cfg)
	}

	if err != nil {
		cancel()
		return err
	}

	t.running = true
	return nil
}

// startDirect connects through TURN to a server listening on a direct address.
func (t *Tunnel) startDirect(ctx context.Context, cfg *TunnelConfig) error {
	serverAddr, err := net.ResolveUDPAddr("udp", cfg.ServerAddr)
	if err != nil {
		return fmt.Errorf("resolve server: %w", err)
	}

	sessionID := uuid.New()

	// 1. Create TURN allocations.
	t.mgr = turn.NewManager(cfg.CallLink, cfg.UseTCP, t.logger)

	allocs, err := t.mgr.Allocate(ctx, cfg.NumConns)
	if err != nil {
		t.mgr.CloseAll()
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
		t.mgr.CloseAll()
		for _, c := range t.cleanups {
			c()
		}
		t.cleanups = nil
		return fmt.Errorf("no DTLS connections established")
	}

	// 3. Create multiplexer.
	t.m = mux.New(t.logger, muxConns...)
	go t.m.DispatchLoop(ctx)
	go t.m.StartPingLoop(ctx, 30*time.Second)

	t.logger.Info("tunnel started (direct)", "connections", len(muxConns), "session_id", sessionID.String())
	return nil
}

// startRelay connects through VK TURN relays to a server that also
// joins the same VK call. Uses VK WebSocket signaling for address exchange.
func (t *Tunnel) startRelay(ctx context.Context, cfg *TunnelConfig) error {
	// 1. Join VK conference.
	jr, err := turn.FetchJoinResponse(ctx, cfg.CallLink)
	if err != nil {
		return fmt.Errorf("join VK conference: %w", err)
	}
	t.logger.Info("joined VK conference", "conv_id", jr.ConvID)

	// 2. Connect to VK signaling.
	sigClient, err := internalsignal.Connect(ctx, jr.WSEndpoint, t.logger.With("component", "signaling"))
	if err != nil {
		return fmt.Errorf("signaling connect: %w", err)
	}

	// 3. Create TURN allocations.
	t.mgr = turn.NewManager(cfg.CallLink, cfg.UseTCP, t.logger)
	allocs, err := t.mgr.Allocate(ctx, cfg.NumConns)
	if err != nil {
		sigClient.Close()
		t.mgr.CloseAll()
		return fmt.Errorf("allocate TURN: %w", err)
	}

	ourAddrs := make([]string, len(allocs))
	for i, a := range allocs {
		ourAddrs[i] = a.RelayAddr.String()
	}

	// 4. Wait for server peer.
	_, err = sigClient.WaitForPeer(ctx)
	if err != nil {
		sigClient.Close()
		t.mgr.CloseAll()
		return fmt.Errorf("wait for peer: %w", err)
	}

	// 5. Exchange relay addresses.
	if err := sigClient.SendRelayAddrs(ctx, ourAddrs, "client"); err != nil {
		sigClient.Close()
		t.mgr.CloseAll()
		return fmt.Errorf("send relay addrs: %w", err)
	}

	serverAddrs, _, err := sigClient.RecvRelayAddrs(ctx)
	if err != nil {
		sigClient.Close()
		t.mgr.CloseAll()
		return fmt.Errorf("recv relay addrs: %w", err)
	}
	sigClient.Close()

	pairCount := len(allocs)
	if len(serverAddrs) < pairCount {
		pairCount = len(serverAddrs)
	}

	// 6. Punch relay.
	for i := 0; i < pairCount; i++ {
		serverUDP, err := net.ResolveUDPAddr("udp", serverAddrs[i])
		if err != nil {
			continue
		}
		internaldtls.PunchRelay(allocs[i].RelayConn, serverUDP)
	}
	time.Sleep(500 * time.Millisecond)

	sessionID := uuid.New()

	// 7. DTLS-over-TURN to server relay addresses.
	var muxConns []io.ReadWriteCloser
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
		t.cleanups = append(t.cleanups, cleanup)

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
		t.mgr.CloseAll()
		for _, c := range t.cleanups {
			c()
		}
		t.cleanups = nil
		return fmt.Errorf("no relay DTLS connections established")
	}

	// 8. Create multiplexer.
	t.m = mux.New(t.logger, muxConns...)
	go t.m.DispatchLoop(ctx)
	go t.m.StartPingLoop(ctx, 30*time.Second)

	t.logger.Info("tunnel started (relay-to-relay)", "connections", len(muxConns), "session_id", sessionID.String())
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
