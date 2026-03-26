package tunnel

import (
	"context"
	"sync"
	"testing"
	"time"
)

type mockSigClient struct {
	mu          sync.Mutex
	sentBatches []sentBatch
	recvQueue   chan recvResult
	alive       bool
	done        chan struct{}
}

type sentBatch struct {
	Addrs []string
	Role  string
	Nonce string
	Batch int
	Final bool
}

type recvResult struct {
	Addrs []string
	Batch int
	Final bool
	Nonce string
	Err   error
}

func newMockSigClient() *mockSigClient {
	return &mockSigClient{
		recvQueue: make(chan recvResult, 16),
		alive:     true,
		done:      make(chan struct{}),
	}
}

func (m *mockSigClient) SendRelayBatch(_ context.Context, addrs []string, role, nonce string, batch int, final bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentBatches = append(m.sentBatches, sentBatch{addrs, role, nonce, batch, final})
	return nil
}

func (m *mockSigClient) RecvRelayBatch(ctx context.Context, _, _ string) ([]string, int, bool, string, error) {
	select {
	case <-ctx.Done():
		return nil, 0, false, "", ctx.Err()
	case r := <-m.recvQueue:
		return r.Addrs, r.Batch, r.Final, r.Nonce, r.Err
	}
}

func (m *mockSigClient) IsAlive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.alive
}

func (m *mockSigClient) Done() <-chan struct{} { return m.done }

func (m *mockSigClient) getSentBatches() []sentBatch {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]sentBatch, len(m.sentBatches))
	copy(cp, m.sentBatches)
	return cp
}

func TestSignalingRouter_BroadcastSendsToAll(t *testing.T) {
	c1 := newMockSigClient()
	c2 := newMockSigClient()
	r := NewSignalingRouter()
	r.Register(c1, "nonce1")
	r.Register(c2, "nonce2")

	ctx := context.Background()
	err := r.BroadcastRelayBatch(ctx, []string{"1.2.3.4:5678"}, "client", "nonce1", 0, true)
	if err != nil {
		t.Fatal(err)
	}

	for i, c := range []*mockSigClient{c1, c2} {
		batches := c.getSentBatches()
		if len(batches) != 1 {
			t.Fatalf("client %d: expected 1 batch, got %d", i, len(batches))
		}
		if batches[0].Nonce != "nonce1" {
			t.Fatalf("client %d: nonce mismatch", i)
		}
		if batches[0].Addrs[0] != "1.2.3.4:5678" {
			t.Fatalf("client %d: addr mismatch", i)
		}
	}
}

func TestSignalingRouter_ReceiveDedup(t *testing.T) {
	c1 := newMockSigClient()
	c2 := newMockSigClient()
	r := NewSignalingRouter()
	r.Register(c1, "nonce1")
	r.Register(c2, "nonce2")

	msg := recvResult{Addrs: []string{"5.6.7.8:1234"}, Batch: 0, Final: true, Nonce: "nonceX"}
	c1.recvQueue <- msg
	c2.recvQueue <- msg

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	addrs, batch, final, nonce, err := r.ReceiveRelayBatch(ctx, "client", "")
	if err != nil {
		t.Fatal(err)
	}
	if nonce != "nonceX" || batch != 0 || !final || addrs[0] != "5.6.7.8:1234" {
		t.Fatalf("unexpected: addrs=%v batch=%d final=%v nonce=%s", addrs, batch, final, nonce)
	}

	// Second receive should block (dedup) until timeout
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	_, _, _, _, err2 := r.ReceiveRelayBatch(ctx2, "client", "")
	if err2 != context.DeadlineExceeded {
		t.Fatalf("expected deadline exceeded, got: %v", err2)
	}
}

func TestSignalingRouter_RemoveClient(t *testing.T) {
	c1 := newMockSigClient()
	r := NewSignalingRouter()
	r.Register(c1, "nonce1")
	if r.ClientCount() != 1 {
		t.Fatal("expected 1 client")
	}

	r.Remove(c1)
	if r.ClientCount() != 0 {
		t.Fatal("expected 0 clients after remove")
	}
}

func TestSignalingRouter_BroadcastNoClients(t *testing.T) {
	r := NewSignalingRouter()
	err := r.BroadcastRelayBatch(context.Background(), []string{"1.2.3.4:5"}, "client", "n", 0, true)
	if err == nil {
		t.Fatal("expected error with no clients")
	}
}

func TestSignalingRouter_ResetDedup(t *testing.T) {
	r := NewSignalingRouter()
	r.seen.Store("test:0", struct{}{})
	r.ResetDedup()

	_, loaded := r.seen.Load("test:0")
	if loaded {
		t.Fatal("expected dedup cleared after reset")
	}
}
