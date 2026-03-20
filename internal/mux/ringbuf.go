package mux

import "sync"

// RawRingBuffer is a thread-safe ring buffer for raw IP packet frames.
// When the buffer is full, the oldest frame is evicted to make room
// for the new one. This ensures that DispatchLoop never blocks — even
// during network stalls or consumer slowdowns, Ping/Pong frames keep
// flowing and connections stay alive.
type RawRingBuffer struct {
	mu     sync.Mutex
	buf    []*Frame
	cap    int
	head   int // index of oldest item
	count  int // number of items in buffer
	notify chan struct{}
	closed bool
}

// NewRawRingBuffer creates a ring buffer with the given capacity.
func NewRawRingBuffer(capacity int) *RawRingBuffer {
	return &RawRingBuffer{
		buf:    make([]*Frame, capacity),
		cap:    capacity,
		notify: make(chan struct{}, 1),
	}
}

// Push adds a frame to the buffer. If the buffer is full, the oldest
// frame is evicted. Returns the number of evicted frames (0 or 1).
// Push never blocks — this is critical to prevent DispatchLoop stalls
// that would starve Ping/Pong processing and kill the tunnel.
func (rb *RawRingBuffer) Push(f *Frame) int {
	rb.mu.Lock()
	if rb.closed {
		rb.mu.Unlock()
		return 0
	}

	evicted := 0
	if rb.count == rb.cap {
		// Overwrite oldest frame.
		rb.buf[rb.head] = f
		rb.head = (rb.head + 1) % rb.cap
		evicted = 1
	} else {
		idx := (rb.head + rb.count) % rb.cap
		rb.buf[idx] = f
		rb.count++
	}
	rb.mu.Unlock()

	// Non-blocking signal to consumer.
	select {
	case rb.notify <- struct{}{}:
	default:
	}
	return evicted
}

// Pop removes and returns the oldest frame. Returns nil, false if empty.
func (rb *RawRingBuffer) Pop() (*Frame, bool) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.count == 0 {
		return nil, false
	}
	f := rb.buf[rb.head]
	rb.buf[rb.head] = nil // help GC
	rb.head = (rb.head + 1) % rb.cap
	rb.count--
	return f, true
}

// Ready returns a channel that is signaled when frames are available.
// The channel is closed when the buffer is closed.
func (rb *RawRingBuffer) Ready() <-chan struct{} {
	return rb.notify
}

// Drain discards all buffered frames and returns the count of discarded frames.
func (rb *RawRingBuffer) Drain() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	n := rb.count
	for i := 0; i < rb.cap; i++ {
		rb.buf[i] = nil
	}
	rb.head = 0
	rb.count = 0
	return n
}

// Len returns the current number of frames in the buffer.
func (rb *RawRingBuffer) Len() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.count
}

// Close marks the buffer as closed and closes the notify channel.
func (rb *RawRingBuffer) Close() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if !rb.closed {
		rb.closed = true
		close(rb.notify)
	}
}

// IsClosed reports whether the buffer has been closed.
func (rb *RawRingBuffer) IsClosed() bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.closed
}
