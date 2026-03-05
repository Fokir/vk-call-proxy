package turn

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/logging"
	pionTurn "github.com/pion/turn/v4"
)

// Allocation represents a single TURN relay allocation.
type Allocation struct {
	Client    *pionTurn.Client
	RelayConn net.PacketConn
	RelayAddr net.Addr
	Creds     *Credentials
	CreatedAt time.Time
	conn      net.Conn
}

// Close tears down the allocation and underlying connection.
func (a *Allocation) Close() error {
	if a.RelayConn != nil {
		a.RelayConn.Close()
	}
	if a.Client != nil {
		a.Client.Close()
	}
	if a.conn != nil {
		a.conn.Close()
	}
	return nil
}

// Manager handles multiple TURN allocations with credential rotation.
type Manager struct {
	mu          sync.Mutex
	allocations []*Allocation
	callLink    string
	useTCP      bool
	logger      *slog.Logger
}

// NewManager creates a TURN allocation manager.
func NewManager(callLink string, useTCP bool, logger *slog.Logger) *Manager {
	return &Manager{
		callLink: callLink,
		useTCP:   useTCP,
		logger:   logger,
	}
}

// Allocate creates n new TURN allocations, each with independently fetched
// anonymous credentials. Returns the successfully created allocations.
func (m *Manager) Allocate(ctx context.Context, n int) ([]*Allocation, error) {
	var (
		mu     sync.Mutex
		allocs []*Allocation
		errs   []error
		wg     sync.WaitGroup
	)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			alloc, err := m.createAllocation(ctx, idx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("allocation %d: %w", idx, err))
				return
			}
			allocs = append(allocs, alloc)
		}(i)
	}
	wg.Wait()

	m.mu.Lock()
	m.allocations = append(m.allocations, allocs...)
	m.mu.Unlock()

	if len(allocs) == 0 && len(errs) > 0 {
		return nil, fmt.Errorf("all allocations failed, first error: %w", errs[0])
	}

	m.logger.Info("TURN allocations created",
		"requested", n,
		"succeeded", len(allocs),
		"failed", len(errs),
	)

	return allocs, nil
}

func (m *Manager) createAllocation(ctx context.Context, idx int) (*Allocation, error) {
	// Each allocation gets fresh credentials (different anonymous identity)
	creds, err := FetchCredentials(ctx, m.callLink)
	if err != nil {
		return nil, fmt.Errorf("fetch credentials: %w", err)
	}

	addr := net.JoinHostPort(creds.Host, creds.Port)
	m.logger.Info("connecting to TURN server",
		"index", idx,
		"addr", addr,
		"username", creds.Username,
	)

	var conn net.Conn
	if m.useTCP {
		d := net.Dialer{Timeout: 10 * time.Second}
		conn, err = d.DialContext(ctx, "tcp", addr)
	} else {
		d := net.Dialer{Timeout: 10 * time.Second}
		conn, err = d.DialContext(ctx, "udp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("dial TURN server: %w", err)
	}

	var turnConn net.PacketConn
	if m.useTCP {
		turnConn = pionTurn.NewSTUNConn(conn)
	} else {
		turnConn = conn.(net.PacketConn)
	}

	cfg := &pionTurn.ClientConfig{
		STUNServerAddr: addr,
		TURNServerAddr: addr,
		Conn:           turnConn,
		Username:       creds.Username,
		Password:       creds.Password,
		LoggerFactory:  logging.NewDefaultLoggerFactory(),
	}

	client, err := pionTurn.NewClient(cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("create TURN client: %w", err)
	}

	if err := client.Listen(); err != nil {
		client.Close()
		conn.Close()
		return nil, fmt.Errorf("TURN listen: %w", err)
	}

	relayConn, err := client.Allocate()
	if err != nil {
		client.Close()
		conn.Close()
		return nil, fmt.Errorf("TURN allocate: %w", err)
	}

	m.logger.Info("TURN allocation succeeded",
		"index", idx,
		"relay_addr", relayConn.LocalAddr().String(),
	)

	return &Allocation{
		Client:    client,
		RelayConn: relayConn,
		RelayAddr: relayConn.LocalAddr(),
		Creds:     creds,
		CreatedAt: time.Now(),
		conn:      conn,
	}, nil
}

// Allocations returns a snapshot of current allocations.
func (m *Manager) Allocations() []*Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Allocation, len(m.allocations))
	copy(out, m.allocations)
	return out
}

// StartKeepalive sends periodic STUN Binding requests on all active
// allocations to keep the TURN control channel alive. VK TURN servers
// appear to have a short idle timeout (~30s) that only resets on STUN
// transactions, not on ChannelData frames. Call in a goroutine.
func (m *Manager) StartKeepalive(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			allocs := make([]*Allocation, len(m.allocations))
			copy(allocs, m.allocations)
			m.mu.Unlock()
			for _, a := range allocs {
				if a != nil && a.Client != nil {
					if _, err := a.Client.SendBindingRequest(); err != nil {
						m.logger.Debug("TURN keepalive failed", "err", err)
					}
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// CloseAll tears down all allocations.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	allocs := m.allocations
	m.allocations = nil
	m.mu.Unlock()

	for _, a := range allocs {
		if err := a.Close(); err != nil {
			m.logger.Warn("error closing allocation", "err", err)
		}
	}
}
