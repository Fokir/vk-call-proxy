package mux

import (
	"container/heap"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"sync"
	"time"
)

// Frame header format (13 bytes):
//   [0:4]   StreamID  uint32  - identifies the logical stream (client connection)
//   [4:5]   Type      uint8   - frame type
//   [5:9]   Sequence  uint32  - monotonic sequence number for ordering
//   [9:13]  Length    uint32  - payload length
//
// Maximum payload: 65535 bytes (fits within TURN relay MTU after overhead).

const (
	headerSize = 13
	maxPayload = 65535
)

// Frame types.
const (
	FrameData  uint8 = 0x01 // Carries user data
	FrameOpen  uint8 = 0x02 // Open a new stream
	FrameClose uint8 = 0x03 // Close stream gracefully
	FramePing  uint8 = 0x04 // Keepalive / latency probe
	FramePong  uint8 = 0x05 // Keepalive response
)

// Frame is a single multiplexed message.
type Frame struct {
	StreamID uint32
	Type     uint8
	Sequence uint32
	Length   uint32
	Payload  []byte

	connIdx int // local-only; set by readLoop, not serialized
}

// MarshalBinary encodes the frame into wire format.
func (f *Frame) MarshalBinary() ([]byte, error) {
	if len(f.Payload) > maxPayload {
		return nil, errors.New("payload exceeds maximum size")
	}
	buf := make([]byte, headerSize+len(f.Payload))
	binary.BigEndian.PutUint32(buf[0:4], f.StreamID)
	buf[4] = f.Type
	binary.BigEndian.PutUint32(buf[5:9], f.Sequence)
	binary.BigEndian.PutUint32(buf[9:13], uint32(len(f.Payload)))
	copy(buf[headerSize:], f.Payload)
	return buf, nil
}

// ReadFrame reads a single frame from r.
func ReadFrame(r io.Reader) (*Frame, error) {
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	f := &Frame{
		StreamID: binary.BigEndian.Uint32(hdr[0:4]),
		Type:     hdr[4],
		Sequence: binary.BigEndian.Uint32(hdr[5:9]),
		Length:   binary.BigEndian.Uint32(hdr[9:13]),
	}
	if f.Length > maxPayload {
		return nil, errors.New("frame length exceeds maximum")
	}
	if f.Length > 0 {
		f.Payload = make([]byte, f.Length)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return nil, err
		}
	}
	return f, nil
}

const retransmitRingSize = 128

// retransmitRing is a fixed-size circular buffer of marshaled frames
// for optimistic retransmission on connection death.
// Push requires external locking (mu); Drain acquires mu internally.
type retransmitRing struct {
	mu    sync.Mutex
	buf   [retransmitRingSize][]byte
	write int
	count int
}

// Push adds a marshaled frame to the ring, evicting the oldest if full.
// Caller MUST hold r.mu.
func (r *retransmitRing) Push(data []byte) {
	r.buf[r.write] = data
	r.write = (r.write + 1) % retransmitRingSize
	if r.count < retransmitRingSize {
		r.count++
	}
}

// Drain returns all buffered frames in FIFO order and resets the ring.
// Thread-safe — acquires r.mu internally.
func (r *retransmitRing) Drain() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count == 0 {
		return nil
	}
	out := make([][]byte, 0, r.count)
	start := (r.write - r.count + retransmitRingSize) % retransmitRingSize
	for i := 0; i < r.count; i++ {
		idx := (start + i) % retransmitRingSize
		out = append(out, r.buf[idx])
		r.buf[idx] = nil // release for GC
	}
	r.count = 0
	r.write = 0
	return out
}

const (
	reorderMaxFrames  = 4096
	reorderGapTimeout = 5 * time.Second
)

type reorderEntry struct {
	stripSeq uint32
	data     []byte
}

// reorderHeap implements heap.Interface for min-heap by stripSeq.
type reorderHeap []reorderEntry

func (h reorderHeap) Len() int           { return len(h) }
func (h reorderHeap) Less(i, j int) bool { return int32(h[i].stripSeq-h[j].stripSeq) < 0 }
func (h reorderHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *reorderHeap) Push(x any)        { *h = append(*h, x.(reorderEntry)) }
func (h *reorderHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// reorderBuffer buffers out-of-order frames and delivers them in StripSeq order.
type reorderBuffer struct {
	h        reorderHeap
	nextSeq  uint32
	gapSince time.Time          // when the current gap started (zero if no gap)
	seenSet  map[uint32]struct{} // for O(1) duplicate check
}

func newReorderBuffer() *reorderBuffer {
	return &reorderBuffer{
		seenSet: make(map[uint32]struct{}),
	}
}

// Insert processes an incoming frame. Returns slice of data payloads ready for
// delivery (in order), or nil if the frame was buffered/duplicate.
func (rb *reorderBuffer) Insert(stripSeq uint32, data []byte) [][]byte {
	// Duplicate: already delivered
	if int32(stripSeq-rb.nextSeq) < 0 {
		return nil
	}
	// Duplicate: already in heap
	if _, ok := rb.seenSet[stripSeq]; ok {
		return nil
	}

	rb.seenSet[stripSeq] = struct{}{}
	heap.Push(&rb.h, reorderEntry{stripSeq: stripSeq, data: data})

	// Start gap timer if this is not the expected next seq
	if stripSeq != rb.nextSeq && rb.gapSince.IsZero() {
		rb.gapSince = time.Now()
	}

	// Flush consecutive frames starting from nextSeq
	var flushed [][]byte
	for rb.h.Len() > 0 && rb.h[0].stripSeq == rb.nextSeq {
		entry := heap.Pop(&rb.h).(reorderEntry)
		delete(rb.seenSet, entry.stripSeq)
		flushed = append(flushed, entry.data)
		rb.nextSeq++
	}

	// Reset gap timer if no gap remains
	if rb.h.Len() == 0 {
		rb.gapSince = time.Time{}
	}

	return flushed
}

// Len returns the number of buffered (not yet delivered) frames.
func (rb *reorderBuffer) Len() int { return rb.h.Len() }

// Overflowed returns true if the buffer exceeds the max frame limit.
func (rb *reorderBuffer) Overflowed() bool { return rb.h.Len() > reorderMaxFrames }

// GapTimedOut returns true if the oldest gap has persisted longer than timeout.
func (rb *reorderBuffer) GapTimedOut(timeout time.Duration) bool {
	if rb.gapSince.IsZero() || rb.h.Len() == 0 {
		return false
	}
	return time.Since(rb.gapSince) > timeout
}

// FlushAll returns all buffered frames in order and resets the buffer.
// Used when draining before stream close.
func (rb *reorderBuffer) FlushAll() [][]byte {
	if rb.h.Len() == 0 {
		return nil
	}
	sort.Slice(rb.h, func(i, j int) bool {
		return int32(rb.h[i].stripSeq-rb.h[j].stripSeq) < 0
	})
	out := make([][]byte, len(rb.h))
	for i, e := range rb.h {
		out[i] = e.data
	}
	rb.h = rb.h[:0]
	rb.seenSet = make(map[uint32]struct{})
	rb.gapSince = time.Time{}
	return out
}
