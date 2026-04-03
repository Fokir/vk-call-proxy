package turn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/pion/logging"
	pionTurn "github.com/pion/turn/v4"
)

// ErrCredentialsExpired is returned when TURN credentials have expired
// (401 Unauthorized or username timestamp in the past). Callers should
// rejoin the conference to obtain fresh credentials.
var ErrCredentialsExpired = errors.New("TURN credentials expired")

// credentialsExpired checks whether TURN credentials have expired by parsing
// the timestamp prefix from the username (format: "expiry:userId").
// Returns true if the credentials expire within the next 60 seconds.
func credentialsExpired(creds *provider.Credentials) bool {
	parts := strings.SplitN(creds.Username, ":", 2)
	if len(parts) < 2 {
		return false // unknown format, assume valid
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix()+60 > expiry // expired or expiring within 60s
}

// CredentialRefreshTimer returns a timer that fires at the earlier of:
// - 5 minutes before TURN credentials expire (based on the username timestamp)
// - maxAllocationAge (4h) to preempt TURN relay allocation limits
// If the expiry cannot be parsed, the timer fires after maxAllocationAge.
func CredentialRefreshTimer(creds *provider.Credentials, logger *slog.Logger) *time.Timer {
	const margin = 5 * time.Minute
	const maxAllocationAge = 4 * time.Hour

	parts := strings.SplitN(creds.Username, ":", 2)
	if len(parts) < 2 {
		logger.Info("session refresh scheduled",
			"refresh_in", maxAllocationAge,
			"reason", "max_allocation_age",
			"note", "unknown credential format, using max age")
		return time.NewTimer(maxAllocationAge)
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		logger.Info("session refresh scheduled",
			"refresh_in", maxAllocationAge,
			"reason", "max_allocation_age",
			"note", "unparseable expiry, using max age")
		return time.NewTimer(maxAllocationAge)
	}

	expiryTime := time.Unix(expiry, 0)
	refreshAt := expiryTime.Add(-margin)
	credDelay := time.Until(refreshAt)
	if credDelay < time.Minute {
		credDelay = time.Minute
	}

	delay := credDelay
	reason := "credential_expiry"
	if maxAllocationAge < delay {
		delay = maxAllocationAge
		reason = "max_allocation_age"
	}

	logger.Info("session refresh scheduled",
		"expiry", expiryTime.Format(time.RFC3339),
		"refresh_in", delay.Round(time.Second),
		"reason", reason)
	return time.NewTimer(delay)
}

// Allocation represents a single TURN relay allocation.
type Allocation struct {
	Client    *pionTurn.Client
	RelayConn net.PacketConn
	RelayAddr net.Addr
	Creds     *provider.Credentials
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
	mu           sync.Mutex
	allocations  []*Allocation
	creds        provider.CredentialsProvider
	initialCreds *provider.Credentials // reuse credentials from initial join (no re-join)
	useTCP       bool
	logger       *slog.Logger
}

// NewManager creates a TURN allocation manager.
// The creds provider is called for each allocation to obtain TURN credentials,
// unless initial credentials are set via SetInitialCredentials.
func NewManager(creds provider.CredentialsProvider, useTCP bool, logger *slog.Logger) *Manager {
	return &Manager{
		creds:  creds,
		useTCP: useTCP,
		logger: logger,
	}
}

// SetInitialCredentials provides pre-fetched credentials from the initial
// conference join. When set, createAllocation reuses these instead of calling
// FetchCredentials (which would re-join the conference and trigger a hangup).
// Same credentials work for multiple TURN allocations on different servers.
func (m *Manager) SetInitialCredentials(creds *provider.Credentials) {
	m.initialCreds = creds
}

// Allocate creates n new TURN allocations, each with independently fetched
// credentials. Returns the successfully created allocations.
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
	var creds *provider.Credentials
	if m.initialCreds != nil && !credentialsExpired(m.initialCreds) {
		// Reuse credentials from initial join — avoids re-joining the conference.
		c := *m.initialCreds
		creds = &c
	} else if m.initialCreds != nil && m.creds != nil {
		// Initial credentials expired but we have a credentials provider —
		// fetch fresh ones. This happens during connection rotation when
		// the original credentials have expired but the session is still alive.
		m.logger.Info("initial credentials expired, fetching fresh ones for reconnect")
		var err error
		creds, err = m.creds.FetchCredentials(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: fetch fresh credentials: %v", ErrCredentialsExpired, err)
		}
	} else if m.initialCreds != nil {
		// Initial credentials expired and no provider — signal caller to rejoin.
		return nil, fmt.Errorf("%w: username %s", ErrCredentialsExpired, m.initialCreds.Username)
	} else {
		// Each allocation gets fresh credentials (different anonymous identity).
		var err error
		creds, err = m.creds.FetchCredentials(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetch credentials: %w", err)
		}
	}
	// Round-robin across available TURN servers to distribute load.
	if len(creds.Servers) > 1 {
		srv := creds.Servers[idx%len(creds.Servers)]
		creds.Host = srv.Host
		creds.Port = srv.Port
	}
	return m.dialAndAllocate(ctx, creds)
}

func (m *Manager) dialAndAllocate(ctx context.Context, creds *provider.Credentials) (*Allocation, error) {
	addr := net.JoinHostPort(creds.Host, creds.Port)
	m.logger.Info("connecting to TURN server",
		"addr", addr,
		"username", creds.Username,
	)

	var err error
	var conn net.Conn
	var turnConn net.PacketConn
	if m.useTCP {
		d := net.Dialer{Timeout: 10 * time.Second}
		conn, err = d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dial TURN server: %w", err)
		}
		// Moderate TCP write buffer for backpressure. Too large (OS default
		// 128-256KB) absorbs bursts and causes TURN relay packet loss. Too
		// small (16KB) bottlenecks throughput. Adaptive bridge pacing
		// handles relay overflow protection.
		if tcpConn, ok := conn.(*net.TCPConn); ok {
			tcpConn.SetNoDelay(true)
			tcpConn.SetWriteBuffer(131072)
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(10 * time.Second)
		}
		turnConn = pionTurn.NewSTUNConn(conn)
	} else {
		turnConn, err = net.ListenPacket("udp4", "")
		if err != nil {
			return nil, fmt.Errorf("listen UDP: %w", err)
		}
		conn = turnConn.(net.Conn)
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
		// Detect expired credentials: TURN returns 401 when the time-limited
		// username has expired. Wrap with sentinel so callers can rejoin.
		errStr := err.Error()
		if strings.Contains(errStr, "401") || strings.Contains(errStr, "Unauthorized") {
			return nil, fmt.Errorf("%w: %v", ErrCredentialsExpired, err)
		}
		return nil, fmt.Errorf("TURN allocate: %w", err)
	}

	m.logger.Info("TURN allocation succeeded",
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

// AllocateWithCredentials creates a single TURN allocation using pre-fetched credentials.
// Used for token-based connections where credentials come from FetchJoinInfoWithToken.
func (m *Manager) AllocateWithCredentials(ctx context.Context, creds *provider.Credentials) (*Allocation, error) {
	alloc, err := m.dialAndAllocate(ctx, creds)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.allocations = append(m.allocations, alloc)
	m.mu.Unlock()
	m.logger.Info("allocated with credentials", "relay", alloc.RelayAddr.String(), "server", net.JoinHostPort(creds.Host, creds.Port))
	return alloc, nil
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
// allocations to keep the TURN TCP connection alive. VK TURN servers
// have a ~90s idle timeout on the control plane. While STUN Binding
// alone may not reset TURN allocation lifetime, it keeps the TCP socket
// active and prevents load balancer timeouts. The actual TURN allocation
// refresh is handled by pion/turn's internal auto-refresh at lifetime/2.
// Call in a goroutine.
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
			for i, a := range allocs {
				if a != nil && a.Client != nil {
					if a.conn != nil {
						a.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
					}
					if _, err := a.Client.SendBindingRequest(); err != nil {
						m.logger.Debug("TURN keepalive failed", "index", i, "err", err)
					}
					if a.conn != nil {
						a.conn.SetWriteDeadline(time.Time{})
					}
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// AllCredentials returns a copy of credentials from all current allocations.
func (m *Manager) AllCredentials() []*provider.Credentials {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*provider.Credentials
	for _, a := range m.allocations {
		if a != nil && a.Creds != nil {
			c := *a.Creds
			out = append(out, &c)
		}
	}
	return out
}

// CloseAll tears down all allocations.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	allocs := m.allocations
	m.allocations = nil
	m.mu.Unlock()

	for i, a := range allocs {
		age := time.Since(a.CreatedAt).Round(time.Second)
		m.logger.Info("closing TURN allocation", "index", i, "age", age, "relay", a.RelayAddr)
		if err := a.Close(); err != nil {
			m.logger.Warn("error closing allocation", "index", i, "err", err)
		}
	}
}

const (
	DefaultBatchSize    = 2
	DefaultBatchDelay   = 2 * time.Second
	MaxImmediateRetries = 3
	ImmediateRetryDelay = 1 * time.Second
	MaxBatchDelay       = 5 * time.Minute
	FloodRetryDelay     = 5 * time.Minute
	MaxNonRLRetries     = 5
)

// GradualOpts configures the batching behavior of AllocateGradual.
type GradualOpts struct {
	BatchSize  int
	BatchDelay time.Duration
}

func (o GradualOpts) withDefaults() GradualOpts {
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.BatchDelay <= 0 {
		o.BatchDelay = DefaultBatchDelay
	}
	return o
}

// BatchResult contains the allocations created in a single batch.
type BatchResult struct {
	Allocs []*Allocation
	Final  bool
}

func (m *Manager) createAllocationWithRetry(ctx context.Context, idx int) (*Allocation, error) {
	for attempt := 0; attempt <= MaxImmediateRetries; attempt++ {
		alloc, err := m.createAllocation(ctx, idx)
		if err == nil {
			return alloc, nil
		}
		rle, isRL := provider.IsRateLimitError(err)
		if !isRL || attempt == MaxImmediateRetries {
			return nil, err
		}
		if rle.Code != 6 && rle.Code != 1105 {
			return nil, err
		}
		m.logger.Warn("rate limit, retrying allocation",
			"index", idx, "attempt", attempt+1, "code", rle.Code, "delay", ImmediateRetryDelay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ImmediateRetryDelay):
		}
	}
	return nil, fmt.Errorf("unreachable")
}

// AllocateGradual creates n allocations in batches and sends each completed batch
// to the returned channel. Failed allocations are retried with increasing delay
// until all n are created or ctx is cancelled. The channel is closed when done.
// The caller MUST drain the channel.
func (m *Manager) AllocateGradual(ctx context.Context, n int, opts GradualOpts) <-chan BatchResult {
	opts = opts.withDefaults()
	ch := make(chan BatchResult, 1)

	go func() {
		defer close(ch)
		currentDelay := opts.BatchDelay

		type pendingItem struct {
			idx     int
			retries int
		}

		pending := make([]pendingItem, n)
		for i := range pending {
			pending[i] = pendingItem{idx: i}
		}
		created := 0

		for len(pending) > 0 {
			batchSize := opts.BatchSize
			if batchSize > len(pending) {
				batchSize = len(pending)
			}
			batch := pending[:batchSize]
			pending = pending[batchSize:]

			m.logger.Info("starting allocation batch",
				"batch_size", len(batch), "pending", len(pending), "created", created, "delay", currentDelay)

			type result struct {
				item  pendingItem
				alloc *Allocation
				err   error
			}
			results := make(chan result, len(batch))
			for _, item := range batch {
				go func(item pendingItem) {
					alloc, err := m.createAllocationWithRetry(ctx, item.idx)
					results <- result{item, alloc, err}
				}(item)
			}

			var batchAllocs []*Allocation
			var retryItems []pendingItem
			escalateDelay := false
			for range batch {
				r := <-results
				if r.err != nil {
					rle, isRL := provider.IsRateLimitError(r.err)
					if isRL {
						m.logger.Warn("allocation rate-limited, will retry",
							"index", r.item.idx, "code", rle.Code, "msg", rle.Message)
						if rle.Code == 14 {
							currentDelay = FloodRetryDelay
							m.logger.Warn("captcha required, setting max delay", "delay", currentDelay)
						} else if rle.Code == 9 || rle.Code == 29 {
							escalateDelay = true
						}
						retryItems = append(retryItems, r.item)
					} else {
						r.item.retries++
						if r.item.retries <= MaxNonRLRetries {
							m.logger.Warn("allocation failed, will retry",
								"index", r.item.idx, "attempt", r.item.retries, "err", r.err)
							retryItems = append(retryItems, r.item)
						} else {
							m.logger.Error("allocation permanently failed",
								"index", r.item.idx, "err", r.err)
						}
					}
					continue
				}
				m.mu.Lock()
				m.allocations = append(m.allocations, r.alloc)
				m.mu.Unlock()
				created++
				batchAllocs = append(batchAllocs, r.alloc)
				m.logger.Info("allocation ready", "index", r.item.idx, "created", created, "total", n)
			}

			pending = append(pending, retryItems...)

			if escalateDelay {
				currentDelay = currentDelay * 2
				if currentDelay > MaxBatchDelay {
					currentDelay = MaxBatchDelay
				}
				m.logger.Warn("escalating batch delay", "new_delay", currentDelay)
			}

			isFinal := len(pending) == 0
			select {
			case ch <- BatchResult{Allocs: batchAllocs, Final: isFinal}:
			case <-ctx.Done():
				m.logger.Info("allocation stopped by context", "created", created, "total", n)
				return
			}

			if !isFinal {
				select {
				case <-ctx.Done():
					m.logger.Info("allocation stopped by context", "created", created, "total", n)
					return
				case <-time.After(currentDelay):
				}
			}
		}

		m.logger.Info("all allocations complete", "created", created, "total", n)
	}()

	return ch
}
