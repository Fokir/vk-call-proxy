package tunnel

import (
	"context"
	"fmt"
	"sync"
)

// sigClient is the subset of provider.SignalingClient used by SignalingRouter.
type sigClient interface {
	SendRelayBatch(ctx context.Context, addrs []string, role string, nonce string, batch int, final bool) error
	RecvRelayBatch(ctx context.Context, skipRole string, filterNonce string) (addrs []string, batch int, final bool, nonce string, err error)
	IsAlive() bool
	Done() <-chan struct{}
}

// SignalingRouter broadcasts relay messages through multiple signaling clients
// and deduplicates incoming messages by nonce+batch.
type SignalingRouter struct {
	mu      sync.Mutex
	clients []sigClient
	nonces  map[string]int // nonce → slot index

	seen sync.Map // "nonce:batch" → struct{} for dedup
}

// NewSignalingRouter creates a new router.
func NewSignalingRouter() *SignalingRouter {
	return &SignalingRouter{
		nonces: make(map[string]int),
	}
}

// Register adds a signaling client with its associated slot nonce.
func (r *SignalingRouter) Register(c sigClient, nonce string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := len(r.clients)
	r.clients = append(r.clients, c)
	r.nonces[nonce] = idx
}

// Remove removes a signaling client.
func (r *SignalingRouter) Remove(c sigClient) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.clients {
		if existing == c {
			r.clients = append(r.clients[:i], r.clients[i+1:]...)
			for nonce, idx := range r.nonces {
				if idx == i {
					delete(r.nonces, nonce)
				} else if idx > i {
					r.nonces[nonce] = idx - 1
				}
			}
			return
		}
	}
}

// ClientCount returns the number of registered clients.
func (r *SignalingRouter) ClientCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.clients)
}

// BroadcastRelayBatch sends relay addresses through ALL registered signaling clients.
// At least one alive client must succeed; errors from individual dead clients are ignored.
func (r *SignalingRouter) BroadcastRelayBatch(ctx context.Context, addrs []string, role, nonce string, batch int, final bool) error {
	r.mu.Lock()
	clients := make([]sigClient, len(r.clients))
	copy(clients, r.clients)
	r.mu.Unlock()

	if len(clients) == 0 {
		return fmt.Errorf("no signaling clients registered")
	}

	var (
		anySuccess bool
		lastErr    error
	)
	for _, c := range clients {
		if !c.IsAlive() {
			continue
		}
		if err := c.SendRelayBatch(ctx, addrs, role, nonce, batch, final); err != nil {
			lastErr = err
		} else {
			anySuccess = true
		}
	}

	if !anySuccess && lastErr != nil {
		return lastErr
	}
	if !anySuccess {
		return fmt.Errorf("no alive signaling clients")
	}
	return nil
}

// ReceiveRelayBatch listens on ALL signaling clients and returns the first unique message.
// Messages are deduplicated by "nonce:batch" key.
func (r *SignalingRouter) ReceiveRelayBatch(ctx context.Context, skipRole, filterNonce string) ([]string, int, bool, string, error) {
	type result struct {
		addrs []string
		batch int
		final bool
		nonce string
		err   error
	}

	ch := make(chan result, 16)

	r.mu.Lock()
	clients := make([]sigClient, len(r.clients))
	copy(clients, r.clients)
	r.mu.Unlock()

	recvCtx, recvCancel := context.WithCancel(ctx)
	defer recvCancel()

	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func(c sigClient) {
			defer wg.Done()
			for {
				select {
				case <-recvCtx.Done():
					return
				default:
				}
				addrs, batch, final, nonce, err := c.RecvRelayBatch(recvCtx, skipRole, filterNonce)
				if err != nil {
					if recvCtx.Err() != nil {
						return
					}
					select {
					case ch <- result{err: err}:
					case <-recvCtx.Done():
					}
					return
				}
				select {
				case ch <- result{addrs, batch, final, nonce, nil}:
				case <-recvCtx.Done():
					return
				}
			}
		}(c)
	}
	go func() { wg.Wait(); close(ch) }()

	for {
		select {
		case <-ctx.Done():
			return nil, 0, false, "", ctx.Err()
		case res, ok := <-ch:
			if !ok {
				return nil, 0, false, "", fmt.Errorf("all signaling clients closed")
			}
			if res.err != nil {
				continue
			}
			key := fmt.Sprintf("%s:%d", res.nonce, res.batch)
			if _, loaded := r.seen.LoadOrStore(key, struct{}{}); loaded {
				continue
			}
			return res.addrs, res.batch, res.final, res.nonce, nil
		}
	}
}

// ResetDedup clears the deduplication state.
func (r *SignalingRouter) ResetDedup() {
	r.seen.Range(func(key, _ any) bool {
		r.seen.Delete(key)
		return true
	})
}
