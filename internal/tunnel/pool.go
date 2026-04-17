package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/turn"

	"github.com/google/uuid"
)

// CallPool manages N CallSlots feeding one shared MUX.
type CallPool struct {
	cfg    PoolConfig
	slots  []*CallSlot
	router *SignalingRouter
	logger *slog.Logger

	mu  sync.Mutex
	m   *mux.Mux
	sid uuid.UUID

	reconnectCh  chan int
	reconnecting sync.Map // slotIdx → struct{}, prevents duplicate reconnects
	done         chan struct{}
	closeOnce    sync.Once
}

// NewCallPool creates a pool with one slot per service.
func NewCallPool(cfg PoolConfig) *CallPool {
	if cfg.ConnsPerCall <= 0 {
		cfg.ConnsPerCall = 4
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &CallPool{
		cfg:         cfg,
		router:      NewSignalingRouter(),
		logger:      cfg.Logger.With("component", "pool"),
		sid:         uuid.New(),
		reconnectCh: make(chan int, len(cfg.Services)),
		done:        make(chan struct{}),
	}
}

// closableCtx derives a context from ctx that is also cancelled when Close() is called.
func (p *CallPool) closableCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-p.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// slotDelay returns the configured slot connect delay, falling back to the default constant.
func (p *CallPool) slotDelay() time.Duration {
	if p.cfg.SlotConnectDelay > 0 {
		return p.cfg.SlotConnectDelay
	}
	return slotConnectDelay
}

// StartClient connects all slots sequentially, allocates connections,
// and returns the shared MUX for proxy to use.
// First slot to establish connections makes the MUX available; remaining continue in background.
func (p *CallPool) StartClient(ctx context.Context) (*mux.Mux, error) {
	ctx, cancel := p.closableCtx(ctx)
	defer cancel()

	p.logger.Info("starting client pool", "calls", len(p.cfg.Services), "conns_per_call", p.cfg.ConnsPerCall)

	p.mu.Lock()
	for i, svc := range p.cfg.Services {
		slot := NewCallSlot(i, svc, RoleClient, p.cfg.UseTCP, p.cfg.AuthToken, p.cfg.VKTokens, p.cfg.Fingerprint, p.logger)
		p.slots = append(p.slots, slot)
	}
	p.mu.Unlock()

	delay := p.slotDelay()

	// Phase 1: Sequential connect (rate limit safe)
	for i, slot := range p.slots {
		if i > 0 {
			p.logger.Info("waiting before next slot connect", "delay", delay)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		if err := slot.Connect(ctx); err != nil {
			p.logger.Error("slot connect failed", "slot", i, "err", err)
			continue
		}
		p.router.Register(slot.SignalingClient(), slot.Nonce())
	}

	if p.router.ClientCount() == 0 {
		return nil, fmt.Errorf("all slots failed to connect")
	}

	// Phase 2: First slot allocates → MUX created → return for proxy
	var firstErr error
	sessionID := p.sid
	for _, slot := range p.slots {
		if slot.SignalingClient() == nil {
			continue
		}

		m, err := slot.AllocateAndConnect(ctx, p.m, p.router, p.cfg.ConnsPerCall, sessionID)
		if err != nil {
			p.logger.Error("slot allocate failed", "slot", slot.index, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		p.mu.Lock()
		if p.m == nil {
			p.m = m
		}
		p.mu.Unlock()

		if p.m != nil {
			// Allocate remaining slots synchronously so that all TURN
			// allocations finish before Start() returns.  On Android the
			// VPN interface is established right after Start(), routing all
			// traffic through the tunnel — any outstanding VK API / TURN
			// requests would then loop back through the unfinished tunnel.
			p.allocateRemaining(ctx, slot.index, sessionID)
			go p.reconnectLoop(ctx)
			go p.monitorSlots(ctx)
			return p.m, nil
		}
	}

	if p.m == nil {
		return nil, fmt.Errorf("no connections established: %v", firstErr)
	}
	return p.m, nil
}

// StartServer connects all slots, pre-allocates TURN, waits for clients.
func (p *CallPool) StartServer(ctx context.Context) (*mux.Mux, error) {
	ctx, cancel := p.closableCtx(ctx)
	defer cancel()

	p.logger.Info("starting server pool", "calls", len(p.cfg.Services), "conns_per_call", p.cfg.ConnsPerCall)

	p.mu.Lock()
	for i, svc := range p.cfg.Services {
		slot := NewCallSlot(i, svc, RoleServer, p.cfg.UseTCP, p.cfg.AuthToken, p.cfg.VKTokens, nil, p.logger)
		p.slots = append(p.slots, slot)
	}
	p.mu.Unlock()

	delay := p.slotDelay()

	// Phase 1: Sequential connect + pre-allocate 1 TURN per slot
	type preAlloc struct {
		slot   *CallSlot
		allocs []*turn.Allocation
	}
	var preAllocs []preAlloc

	for i, slot := range p.slots {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		if err := slot.Connect(ctx); err != nil {
			p.logger.Error("slot connect failed", "slot", i, "err", err)
			continue
		}
		p.router.Register(slot.SignalingClient(), slot.Nonce())

		if sc := slot.SignalingClient(); sc != nil {
			sc.Drain()
		}

		allocs, err := slot.PreAllocate(ctx, 1)
		if err != nil {
			p.logger.Warn("pre-allocate failed", "slot", i, "err", err)
		} else {
			preAllocs = append(preAllocs, preAlloc{slot, allocs})
		}
	}

	if p.router.ClientCount() == 0 {
		return nil, fmt.Errorf("all slots failed to connect")
	}

	// Build pre-alloc lookup by slot index
	preAllocMap := make(map[int][]*turn.Allocation)
	for _, pa := range preAllocs {
		preAllocMap[pa.slot.index] = pa.allocs
	}

	// Phase 2: Wait for client relay addrs, exchange, DTLS.
	// Sequential with per-slot timeout to avoid MUX creation race
	// while still preventing indefinite blocking.
	for _, slot := range p.slots {
		if slot.SignalingClient() == nil {
			continue
		}

		slotCtx, slotCancel := context.WithTimeout(ctx, serverSlotTimeout)
		m, err := slot.AllocateAndConnectServer(slotCtx, p.m, p.router, p.cfg.ConnsPerCall, preAllocMap[slot.index])
		slotCancel()

		if err != nil {
			p.logger.Error("slot allocate failed", "slot", slot.index, "err", err)
			p.queueReconnect(slot.index)
			continue
		}

		p.mu.Lock()
		if p.m == nil {
			p.m = m
		}
		p.mu.Unlock()
	}

	if p.m == nil {
		return nil, fmt.Errorf("no connections established")
	}

	go p.reconnectLoop(ctx)
	go p.monitorSlots(ctx)
	return p.m, nil
}

func (p *CallPool) allocateRemaining(ctx context.Context, skipIndex int, sessionID uuid.UUID) {
	for _, slot := range p.slots {
		if slot.index == skipIndex || slot.SignalingClient() == nil {
			continue
		}

		p.mu.Lock()
		m := p.m
		p.mu.Unlock()

		_, err := slot.AllocateAndConnect(ctx, m, p.router, p.cfg.ConnsPerCall, sessionID)
		if err != nil {
			p.logger.Error("background slot allocate failed", "slot", slot.index, "err", err)
			p.queueReconnect(slot.index)
		}
	}
}

func (p *CallPool) monitorSlots(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case <-ticker.C:
			p.mu.Lock()
			slots := make([]*CallSlot, len(p.slots))
			copy(slots, p.slots)
			m := p.m
			p.mu.Unlock()

			for _, slot := range slots {
				if slot.State() != SlotActive {
					continue
				}

				// Check 1: signaling death
				sc := slot.SignalingClient()
				if sc != nil && !sc.IsAlive() {
					p.logger.Warn("slot signaling died, queuing reconnect", "slot", slot.index)
					p.queueReconnect(slot.index)
					continue
				}

				// Check 2: all DTLS connections dead
				if m != nil && m.ActiveConns() == 0 {
					p.logger.Warn("slot all DTLS conns dead, queuing reconnect", "slot", slot.index)
					p.queueReconnect(slot.index)
				}
			}
		}
	}
}

func (p *CallPool) queueReconnect(slotIndex int) {
	select {
	case p.reconnectCh <- slotIndex:
	default:
	}
}

func (p *CallPool) reconnectLoop(ctx context.Context) {
	var delays sync.Map // slotIdx → time.Duration

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case slotIdx := <-p.reconnectCh:
			if slotIdx < 0 || slotIdx >= len(p.slots) {
				continue
			}
			// Skip if this slot is already being reconnected.
			if _, loaded := p.reconnecting.LoadOrStore(slotIdx, struct{}{}); loaded {
				continue
			}
			go p.reconnectSlot(ctx, slotIdx, &delays)
		}
	}
}

func (p *CallPool) reconnectSlot(ctx context.Context, slotIdx int, delays *sync.Map) {
	defer p.reconnecting.Delete(slotIdx)

	// Determine backoff delay.
	delay := reconnectInitDelay
	if v, ok := delays.Load(slotIdx); ok {
		if d := v.(time.Duration); d > 0 {
			delay = d
		}
	}

	p.logger.Info("reconnecting slot", "slot", slotIdx, "delay", delay)

	select {
	case <-ctx.Done():
		return
	case <-p.done:
		return
	case <-time.After(delay):
	}

	// Read old slot under lock.
	p.mu.Lock()
	slot := p.slots[slotIdx]
	p.mu.Unlock()

	// Remove old signaling client from router before closing.
	if sc := slot.SignalingClient(); sc != nil {
		p.router.Remove(sc)
	}
	slot.Close()

	newSlot := NewCallSlot(slotIdx, slot.svc, slot.role, slot.useTCP, slot.authToken, slot.vkTokens, slot.fp, p.logger)

	p.mu.Lock()
	p.slots[slotIdx] = newSlot
	p.mu.Unlock()

	if err := newSlot.Connect(ctx); err != nil {
		p.logger.Error("reconnect: connect failed", "slot", slotIdx, "err", err)
		// Linear backoff: 3s, 6s, 9s... cap 60s
		delays.Store(slotIdx, min(delay+reconnectInitDelay, reconnectMaxDelay))
		p.queueReconnect(slotIdx)
		return
	}

	p.router.Register(newSlot.SignalingClient(), newSlot.Nonce())

	p.mu.Lock()
	m := p.m
	p.mu.Unlock()

	allocCtx, allocCancel := context.WithTimeout(ctx, reconnectSlotTimeout)
	defer allocCancel()

	sessionID := p.sid
	if newSlot.role == RoleClient {
		_, err := newSlot.AllocateAndConnect(allocCtx, m, p.router, p.cfg.ConnsPerCall, sessionID)
		if err != nil {
			p.logger.Error("reconnect: allocate failed", "slot", slotIdx, "err", err)
			delays.Store(slotIdx, min(delay+reconnectInitDelay, reconnectMaxDelay))
			p.queueReconnect(slotIdx)
			return
		}
	} else {
		_, err := newSlot.AllocateAndConnectServer(allocCtx, m, p.router, p.cfg.ConnsPerCall, nil)
		if err != nil {
			p.logger.Error("reconnect: allocate failed", "slot", slotIdx, "err", err)
			delays.Store(slotIdx, min(delay+reconnectInitDelay, reconnectMaxDelay))
			p.queueReconnect(slotIdx)
			return
		}
	}

	delays.Store(slotIdx, time.Duration(0))
	p.router.ResetDedup() // clear stale nonce entries from previous session
	p.logger.Info("reconnect successful", "slot", slotIdx)
}

// Mux returns the shared MUX.
func (p *CallPool) Mux() *mux.Mux {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m
}

// Status returns health of all slots.
func (p *CallPool) Status() []SlotStatus {
	statuses := make([]SlotStatus, len(p.slots))
	for i, slot := range p.slots {
		statuses[i] = slot.Status()
	}
	return statuses
}

// Close gracefully shuts down the pool.
func (p *CallPool) Close() {
	p.closeOnce.Do(func() {
		close(p.done)
		p.mu.Lock()
		slots := make([]*CallSlot, len(p.slots))
		copy(slots, p.slots)
		m := p.m
		p.m = nil
		p.mu.Unlock()
		for _, slot := range slots {
			slot.Close()
		}
		if m != nil {
			m.Close()
		}
	})
}
