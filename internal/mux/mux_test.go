package mux

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"testing"
)

// dummyConn is a minimal ReadWriteCloser for testing.
type dummyConn struct{ bytes.Buffer }

func (d *dummyConn) Close() error               { return nil }
func (d *dummyConn) Read(p []byte) (int, error) { return 0, nil }

func TestSelectConn_LatencyThreshold(t *testing.T) {
	m := newTestMux(3)

	// Set latencies: conn0=10ms, conn1=20ms, conn2=100ms (>3x of 10ms=30ms)
	m.conns[0].stats.latency.Store(10_000_000)  // 10ms
	m.conns[1].stats.latency.Store(20_000_000)  // 20ms
	m.conns[2].stats.latency.Store(100_000_000) // 100ms

	// Run selectConn 100 times, conn2 should never be selected
	counts := map[*muxConn]int{}
	for i := 0; i < 100; i++ {
		mc := m.selectConn()
		counts[mc]++
	}
	if counts[m.conns[2]] > 0 {
		t.Fatalf("conn2 (100ms) should be excluded, got %d selections", counts[m.conns[2]])
	}
	if counts[m.conns[0]] == 0 || counts[m.conns[1]] == 0 {
		t.Fatal("conn0 and conn1 should both be selected")
	}
}

func TestSelectConn_AllZeroLatency(t *testing.T) {
	m := newTestMux(3)
	// Should not exclude all — fallback to all conns
	mc := m.selectConn()
	if mc == nil {
		t.Fatal("selectConn should return a conn even with zero latencies")
	}
}

func newTestMux(n int) *Mux {
	m := &Mux{
		inFrames: make(chan *Frame, 100),
		allDead:  make(chan struct{}),
		connDied: make(chan int, 16),
		logger:   slog.Default(),
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	for i := 0; i < n; i++ {
		m.conns = append(m.conns, &muxConn{conn: &dummyConn{}})
	}
	return m
}

func TestFrameClose_DeferredWithPendingReorder(t *testing.T) {
	m := newTestMux(2)

	s := &Stream{
		ID:       1,
		mux:      m,
		recv:     make(chan []byte, 1024),
		done:     make(chan struct{}),
		striping: true,
		reorder:  newReorderBuffer(),
	}
	m.streams.Store(uint32(1), s)

	// Insert out-of-order frame (seq 1, gap at seq 0)
	m.dispatch(&Frame{StreamID: 1, Type: FrameData,
		Payload: append(binary.BigEndian.AppendUint32(nil, 1), []byte("data1")...)})

	// Send FrameClose — should be deferred
	m.dispatch(&Frame{StreamID: 1, Type: FrameClose})
	if s.closed.Load() {
		t.Fatal("stream should not be closed yet — reorder buffer has pending frames")
	}
	if !s.closePending.Load() {
		t.Fatal("closePending should be set")
	}

	// Fill the gap (seq 0) — should flush both frames and execute deferred close
	m.dispatch(&Frame{StreamID: 1, Type: FrameData,
		Payload: append(binary.BigEndian.AppendUint32(nil, 0), []byte("data0")...)})

	// Stream should now be closed
	if !s.closed.Load() {
		t.Fatal("stream should be closed after reorder buffer drained")
	}

	// Verify data was delivered in order
	d0 := <-s.recv
	if string(d0) != "data0" {
		t.Fatalf("expected 'data0', got '%s'", d0)
	}
	d1 := <-s.recv
	if string(d1) != "data1" {
		t.Fatalf("expected 'data1', got '%s'", d1)
	}
}

func TestNonStripedStream_BackwardCompat(t *testing.T) {
	m := newTestMux(1)

	s := &Stream{
		ID:           1,
		mux:          m,
		recv:         make(chan []byte, 1024),
		done:         make(chan struct{}),
		assignedConn: m.conns[0],
		striping:     m.TotalConns() > 1,
	}
	if s.striping {
		t.Fatal("should not be striped with 1 conn")
	}
	if s.reorder != nil {
		t.Fatal("reorder buffer should be nil for non-striped stream")
	}

	m.streams.Store(uint32(1), s)

	// Dispatch data without StripSeq prefix — should work as before
	m.dispatch(&Frame{StreamID: 1, Type: FrameData, Payload: []byte("hello")})
	select {
	case data := <-s.recv:
		if string(data) != "hello" {
			t.Fatalf("expected 'hello', got '%s'", data)
		}
	default:
		t.Fatal("expected data in recv channel")
	}
}
