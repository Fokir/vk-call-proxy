package mux

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ErrMuxClosed is returned when the mux has been closed.
var ErrMuxClosed = errors.New("mux closed")

// pingPayloadSize is the size of the Ping/Pong payload: 8 bytes timestamp + 4 bytes connIdx.
const pingPayloadSize = 12

// encodePingPayload encodes a timestamp and connection index into a Ping payload.
func encodePingPayload(ts time.Time, connIdx int) []byte {
	buf := make([]byte, pingPayloadSize)
	binary.BigEndian.PutUint64(buf[0:8], uint64(ts.UnixNano()))
	binary.BigEndian.PutUint32(buf[8:12], uint32(connIdx))
	return buf
}

// decodePingPayload decodes the timestamp and connection index from a Ping/Pong payload.
func decodePingPayload(p []byte) (ts int64, connIdx int, ok bool) {
	if len(p) < pingPayloadSize {
		return 0, 0, false
	}
	ts = int64(binary.BigEndian.Uint64(p[0:8]))
	connIdx = int(binary.BigEndian.Uint32(p[8:12]))
	return ts, connIdx, true
}

// StartPingLoop sends periodic ping frames on EVERY underlying connection
// to keep TURN allocations alive and measure per-connection latency.
// Each Ping carries a timestamp + connection index in its payload;
// the peer echoes it back in Pong, allowing RTT measurement.
func (m *Mux) StartPingLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			conns := make([]*muxConn, len(m.conns))
			copy(conns, m.conns)
			m.mu.Unlock()

			now := time.Now()
			var wg sync.WaitGroup
			for i, mc := range conns {
				if mc == nil {
					continue
				}
				f := &Frame{
					StreamID: 0,
					Type:     FramePing,
					Sequence: m.NextSeq(),
					Payload:  encodePingPayload(now, i),
				}
				f.Length = uint32(len(f.Payload))
				data, err := f.MarshalBinary()
				if err != nil {
					continue
				}
				wg.Add(1)
				go func(mc *muxConn, data []byte) {
					defer wg.Done()
					mc.mu.Lock()
					if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
						d.SetWriteDeadline(time.Now().Add(5 * time.Second))
					}
					mc.conn.Write(data)
					if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
						d.SetWriteDeadline(time.Time{})
					}
					mc.mu.Unlock()
				}(mc, data)
			}
			wg.Wait()
		case <-ctx.Done():
			return
		}
	}
}

// connStats tracks per-connection quality metrics.
type connStats struct {
	latency   atomic.Int64 // nanoseconds, rolling average
	bytesSent atomic.Int64
	errors    atomic.Int64
	lastUsed  atomic.Int64 // unix nano
}

// Mux multiplexes framed streams over multiple underlying connections,
// distributing load based on measured connection quality.
type Mux struct {
	mu      sync.Mutex // protects conns
	conns   []*muxConn
	streams sync.Map // streamID -> *Stream
	nextSeq atomic.Uint32
	logger  *slog.Logger

	inFrames      chan *Frame
	ctx           context.Context
	cancel        context.CancelFunc
	activeReaders atomic.Int32
	allDead        chan struct{}
	allDeadOnce    sync.Once
	closeInFrames  sync.Once
	idleTimeout    time.Duration // 0 = no idle timeout

	rawPackets   *RawRingBuffer // if set, StreamID=0 FrameData is routed here by DispatchLoop

	acceptedStreams   chan *Stream // DispatchLoop pushes FrameOpen streams here
	closeAcceptOnce  sync.Once

	connDied     chan int        // index of dead connection, buffered
	reconnecting atomic.Int32   // active reconnect count; prevents premature allDead close

	lastPongAt atomic.Int64 // unix nano of last received Pong on any connection

	maxStreams   int          // 0 = unlimited
	streamCount atomic.Int32 // current active stream count
}

type muxConn struct {
	conn       io.ReadWriteCloser
	stats      connStats
	mu         sync.Mutex // serializes writes
	wrrCurrent int64      // smooth weighted round-robin current weight (protected by Mux.mu)
}

// New creates a multiplexer over the given connections.
// Can be called with zero connections; use AddConn to add them later.
func New(logger *slog.Logger, conns ...io.ReadWriteCloser) *Mux {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mux{
		logger:   logger,
		inFrames: make(chan *Frame, 256),
		ctx:      ctx,
		cancel:   cancel,
		allDead:  make(chan struct{}),
		connDied: make(chan int, 32),
	}
	for _, c := range conns {
		mc := &muxConn{conn: c}
		mc.stats.lastUsed.Store(time.Now().UnixNano())
		m.conns = append(m.conns, mc)
	}
	// Start reader goroutines for each connection.
	for i, mc := range m.conns {
		m.activeReaders.Add(1)
		go m.readLoop(i, mc)
	}
	return m
}

// SetIdleTimeout configures a read deadline on connections.
// If no data arrives within the timeout, the readLoop closes.
// The peer's ping loop (default 30s) prevents false triggers.
func (m *Mux) SetIdleTimeout(d time.Duration) {
	m.idleTimeout = d
}

// SetMaxStreams sets the maximum number of concurrent streams.
// 0 means unlimited (default). Must be called before DispatchLoop.
func (m *Mux) SetMaxStreams(n int) {
	m.maxStreams = n
}

// readLoop reads frames from a single underlying connection.
// DTLS is message-oriented, so we wrap the connection in a bufio.Reader
// to provide stream semantics for ReadFrame's io.ReadFull calls.
func (m *Mux) readLoop(idx int, mc *muxConn) {
	defer func() {
		mc.conn.Close() // close DTLS connection immediately to free resources

		// Notify about dead connection (non-blocking).
		select {
		case m.connDied <- idx:
		default:
		}

		if m.activeReaders.Add(-1) == 0 && m.reconnecting.Load() == 0 {
			m.allDeadOnce.Do(func() { close(m.allDead) })
			m.closeInFrames.Do(func() { close(m.inFrames) })
		}
	}()
	br := bufio.NewReaderSize(mc.conn, 16384)
	for {
		if m.idleTimeout > 0 {
			if d, ok := mc.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
				d.SetReadDeadline(time.Now().Add(m.idleTimeout))
			}
		}
		f, err := ReadFrame(br)
		if err != nil {
			if m.ctx.Err() != nil {
				return
			}
			mc.stats.errors.Add(1)
			m.logger.Warn("read error on connection", "index", idx, "err", err)
			return
		}
		f.connIdx = idx
		mc.stats.lastUsed.Store(time.Now().UnixNano())
		select {
		case m.inFrames <- f:
		case <-m.ctx.Done():
			return
		}
	}
}

// selectConn picks the best connection based on quality metrics.
// Strategy: weighted by inverse latency, preferring connections with fewer errors.
// selectConn picks a connection using smooth weighted round-robin.
// Connections with lower latency get proportionally more traffic.
// Algorithm (nginx-style SWRR):
//  1. For each live conn: currentWeight += effectiveWeight
//  2. Pick conn with highest currentWeight
//  3. Winner's currentWeight -= totalWeight
func (m *Mux) selectConn() *muxConn {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.conns) == 0 {
		return nil
	}

	var best *muxConn
	bestCW := int64(math.MinInt64)
	var totalWeight int64

	for _, mc := range m.conns {
		if mc == nil {
			continue
		}
		lat := mc.stats.latency.Load()
		if lat == 0 {
			lat = 1_000_000 // 1ms default
		}
		errs := mc.stats.errors.Load()

		// Weight: inverse latency (higher = better), penalty for errors.
		weight := 1_000_000_000/lat - errs*10
		if weight < 1 {
			weight = 1
		}

		mc.wrrCurrent += weight
		totalWeight += weight

		if mc.wrrCurrent > bestCW {
			bestCW = mc.wrrCurrent
			best = mc
		}
	}

	if best != nil {
		best.wrrCurrent -= totalWeight
	}
	return best
}

// SendFrame writes a frame using the SWRR-selected connection.
// Use this for non-stream frames (raw packets, pings, control).
func (m *Mux) SendFrame(f *Frame) error {
	return m.sendFrameOn(m.selectConn(), f)
}

// sendFrameOn writes a frame on a specific connection.
// If mc is nil or dead, falls back to selectConn.
func (m *Mux) sendFrameOn(mc *muxConn, f *Frame) error {
	if mc == nil {
		mc = m.selectConn()
	}
	if mc == nil {
		return errors.New("no connections available")
	}

	data, err := f.MarshalBinary()
	if err != nil {
		return err
	}

	mc.mu.Lock()
	if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		d.SetWriteDeadline(time.Now().Add(5 * time.Second))
	}
	_, err = mc.conn.Write(data)
	if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		d.SetWriteDeadline(time.Time{})
	}
	mc.mu.Unlock()

	if err != nil {
		mc.stats.errors.Add(1)
		// Try fallback to another connection.
		fallback := m.selectConn()
		if fallback != nil && fallback != mc {
			mc = fallback
			mc.mu.Lock()
			if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
				d.SetWriteDeadline(time.Now().Add(5 * time.Second))
			}
			_, err = mc.conn.Write(data)
			if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
				d.SetWriteDeadline(time.Time{})
			}
			mc.mu.Unlock()
			if err != nil {
				mc.stats.errors.Add(1)
				return err
			}
			mc.stats.bytesSent.Add(int64(len(data)))
			mc.stats.lastUsed.Store(time.Now().UnixNano())
			return nil
		}
		return err
	}
	mc.stats.bytesSent.Add(int64(len(data)))
	mc.stats.lastUsed.Store(time.Now().UnixNano())
	return nil
}

// RecvFrames returns the channel of incoming frames.
func (m *Mux) RecvFrames() <-chan *Frame {
	return m.inFrames
}

// EnableRawPackets creates a ring buffer for raw IP packets
// (StreamID=0, FrameData). DispatchLoop routes matching frames here
// instead of dispatching them as stream data. When the buffer is full,
// the oldest frame is evicted (not the newest), so fresh responses
// take priority over stale data. Must be called before DispatchLoop.
func (m *Mux) EnableRawPackets(bufSize int) {
	m.rawPackets = NewRawRingBuffer(bufSize)
}

// RawPackets returns the ring buffer for raw IP packets, or nil if
// EnableRawPackets was not called.
func (m *Mux) RawPackets() *RawRingBuffer {
	return m.rawPackets
}

// EnableStreamAccept creates a channel for accepted streams.
// When DispatchLoop encounters a FrameOpen, the new stream is pushed here.
// Must be called before DispatchLoop.
func (m *Mux) EnableStreamAccept(bufSize int) {
	m.acceptedStreams = make(chan *Stream, bufSize)
}

// AcceptedStreams returns the channel of streams opened by the remote peer,
// or nil if EnableStreamAccept was not called.
func (m *Mux) AcceptedStreams() <-chan *Stream {
	return m.acceptedStreams
}

// NextSeq returns a monotonically increasing sequence number.
func (m *Mux) NextSeq() uint32 {
	return m.nextSeq.Add(1)
}

// UpdateLatency reports a measured RTT for a connection index.
func (m *Mux) UpdateLatency(idx int, rtt time.Duration) {
	m.mu.Lock()
	if idx < 0 || idx >= len(m.conns) {
		m.mu.Unlock()
		return
	}
	mc := m.conns[idx]
	m.mu.Unlock()
	if mc == nil {
		return
	}
	old := mc.stats.latency.Load()
	if old == 0 {
		mc.stats.latency.Store(rtt.Nanoseconds())
	} else {
		// Exponential moving average (alpha=0.3)
		newVal := int64(float64(old)*0.7 + float64(rtt.Nanoseconds())*0.3)
		mc.stats.latency.Store(newVal)
	}
}

// AddConn dynamically adds a new connection to the multiplexer.
// A new readLoop goroutine is spawned for the connection.
// This is used by the server to add DTLS connections as they arrive
// for a given session. Reuses nil slots left by RemoveConn to prevent
// unbounded growth of the connection slice.
func (m *Mux) AddConn(conn io.ReadWriteCloser) {
	mc := &muxConn{conn: conn}
	mc.stats.lastUsed.Store(time.Now().UnixNano())

	m.mu.Lock()
	idx := -1
	for i, c := range m.conns {
		if c == nil {
			idx = i
			m.conns[i] = mc
			break
		}
	}
	if idx == -1 {
		idx = len(m.conns)
		m.conns = append(m.conns, mc)
	}
	m.mu.Unlock()

	m.activeReaders.Add(1)
	go m.readLoop(idx, mc)
}

// ProbeConnections sends an immediate ping on all connections and sets
// a short read deadline so that dead connections are detected quickly.
// Live connections will respond with pong, resetting the normal idle
// timeout on the next readLoop iteration. Dead connections will timeout
// after probeTimeout and trigger ConnDied via the normal readLoop exit path.
func (m *Mux) ProbeConnections(probeTimeout time.Duration) {
	f := &Frame{StreamID: 0, Type: FramePing, Sequence: m.NextSeq()}
	data, err := f.MarshalBinary()
	if err != nil {
		return
	}

	m.mu.Lock()
	conns := make([]*muxConn, len(m.conns))
	copy(conns, m.conns)
	m.mu.Unlock()

	for _, mc := range conns {
		if mc == nil {
			continue
		}
		mc.mu.Lock()
		if d, ok := mc.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			d.SetReadDeadline(time.Now().Add(probeTimeout))
		}
		if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			d.SetWriteDeadline(time.Now().Add(probeTimeout))
		}
		mc.conn.Write(data)
		if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			d.SetWriteDeadline(time.Time{})
		}
		mc.mu.Unlock()
	}
}

// Dead returns a channel that is closed when all readLoop goroutines
// have exited (all underlying connections are dead).
func (m *Mux) Dead() <-chan struct{} {
	return m.allDead
}

// ConnDied returns a channel that receives the index of each connection
// whose readLoop has exited. Use this to trigger per-connection reconnects.
func (m *Mux) ConnDied() <-chan int {
	return m.connDied
}

// IsHealthy reports whether at least one Pong was received within the
// given timeout. If no Pong has ever been received, the mux creation time
// is used as baseline so the check doesn't fire during initial handshake.
func (m *Mux) IsHealthy(timeout time.Duration) bool {
	last := m.lastPongAt.Load()
	if last == 0 {
		// No pong yet — healthy as long as connections exist.
		return m.activeReaders.Load() > 0
	}
	return time.Since(time.Unix(0, last)) < timeout
}

// ActiveConns returns the number of readLoop goroutines still running.
func (m *Mux) ActiveConns() int {
	return int(m.activeReaders.Load())
}

// TotalConns returns the number of non-nil connection slots.
func (m *Mux) TotalConns() int {
	m.mu.Lock()
	n := 0
	for _, c := range m.conns {
		if c != nil {
			n++
		}
	}
	m.mu.Unlock()
	return n
}

// RemoveConn sets the connection at the given index to nil.
// The slot is reused by subsequent AddConn calls.
func (m *Mux) RemoveConn(idx int) {
	m.mu.Lock()
	if idx >= 0 && idx < len(m.conns) {
		m.conns[idx] = nil
	}
	m.mu.Unlock()
}

// BeginReconnect increments the reconnecting counter, preventing
// the mux from closing allDead while reconnection is in progress.
func (m *Mux) BeginReconnect() {
	m.reconnecting.Add(1)
}

// EndReconnect decrements the reconnecting counter. If no active
// readers remain and no reconnects are pending, allDead is closed.
func (m *Mux) EndReconnect() {
	if m.reconnecting.Add(-1) <= 0 && m.activeReaders.Load() == 0 {
		m.allDeadOnce.Do(func() { close(m.allDead) })
		m.closeInFrames.Do(func() { close(m.inFrames) })
	}
}

// Close shuts down the multiplexer and all underlying connections.
func (m *Mux) Close() error {
	m.cancel()
	m.closeInFrames.Do(func() { close(m.inFrames) })
	if m.rawPackets != nil {
		m.rawPackets.Close()
	}
	if m.acceptedStreams != nil {
		m.closeAcceptOnce.Do(func() { close(m.acceptedStreams) })
	}
	m.mu.Lock()
	conns := m.conns
	m.mu.Unlock()
	for _, mc := range conns {
		if mc != nil {
			mc.conn.Close()
		}
	}
	return nil
}

// Stream represents a logical bidirectional stream within the mux.
type Stream struct {
	ID           uint32
	mux          *Mux
	recv         chan []byte
	closed       atomic.Bool
	done         chan struct{}
	leftover     []byte    // unread remainder from previous Read
	assignedConn *muxConn  // pinned connection for write ordering (set once at creation)
}

// OpenStream creates a new logical stream and sends an open frame to the peer.
// The stream is pinned to a connection chosen via SWRR for write ordering.
func (m *Mux) OpenStream(id uint32) (*Stream, error) {
	assigned := m.selectConn()
	s := &Stream{
		ID:           id,
		mux:          m,
		recv:         make(chan []byte, 1024),
		done:         make(chan struct{}),
		assignedConn: assigned,
	}
	m.streamCount.Add(1)
	m.streams.Store(id, s)

	err := m.sendFrameOn(assigned, &Frame{
		StreamID: id,
		Type:     FrameOpen,
		Sequence: m.NextSeq(),
	})
	if err != nil {
		m.streams.Delete(id)
		m.streamCount.Add(-1)
		return nil, err
	}
	return s, nil
}

// AcceptStream returns a stream when an open frame is received.
func (m *Mux) AcceptStream(ctx context.Context) (*Stream, error) {
	for {
		select {
		case f, ok := <-m.inFrames:
			if !ok {
				return nil, ErrMuxClosed
			}
			if f.Type == FrameOpen {
				if m.maxStreams > 0 && int(m.streamCount.Load()) >= m.maxStreams {
					m.logger.Warn("max streams reached, rejecting", "stream", f.StreamID, "max", m.maxStreams)
					continue
				}
				s := &Stream{
					ID:   f.StreamID,
					mux:  m,
					recv: make(chan []byte, 1024),
					done: make(chan struct{}),
				}
				m.streamCount.Add(1)
				m.streams.Store(f.StreamID, s)
				return s, nil
			}
			// Dispatch data/close frames to existing streams.
			m.dispatch(f)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// dispatch routes a received frame to the appropriate stream.
func (m *Mux) dispatch(f *Frame) {
	val, ok := m.streams.Load(f.StreamID)
	if !ok {
		return
	}
	s := val.(*Stream)
	switch f.Type {
	case FrameData:
		select {
		case s.recv <- f.Payload:
		case <-s.done:
		}
	case FrameClose:
		s.closed.Store(true)
		close(s.recv)
		m.streams.Delete(f.StreamID)
		m.streamCount.Add(-1)
	}
}

// DispatchLoop processes incoming frames and routes them to streams.
// Call this in a goroutine on the client/server side.
// If EnableRawPackets was called, StreamID=0 FrameData frames are
// forwarded to the RawPackets channel instead of being dispatched.
// If EnableStreamAccept was called, new streams from FrameOpen are
// pushed to AcceptedStreams channel.
func (m *Mux) DispatchLoop(ctx context.Context) {
	defer func() {
		if m.rawPackets != nil {
			m.rawPackets.Close()
		}
		if m.acceptedStreams != nil {
			m.closeAcceptOnce.Do(func() { close(m.acceptedStreams) })
		}
	}()
	for {
		select {
		case f, ok := <-m.inFrames:
			if !ok {
				return
			}
			// Handle Ping/Pong keepalive with per-connection latency tracking.
			if f.Type == FramePing {
				// Echo the payload back so the sender can measure RTT.
				pong := &Frame{
					StreamID: 0,
					Type:     FramePong,
					Sequence: m.NextSeq(),
					Payload:  f.Payload,
					Length:   uint32(len(f.Payload)),
				}
				// Respond on the same connection that received the Ping.
				m.mu.Lock()
				var mc *muxConn
				if f.connIdx >= 0 && f.connIdx < len(m.conns) {
					mc = m.conns[f.connIdx]
				}
				m.mu.Unlock()
				_ = m.sendFrameOn(mc, pong)
				continue
			}
			if f.Type == FramePong {
				m.lastPongAt.Store(time.Now().UnixNano())
				// Decode embedded timestamp + connIdx to measure per-connection RTT.
				if ts, connIdx, ok := decodePingPayload(f.Payload); ok {
					rtt := time.Since(time.Unix(0, ts))
					if rtt > 0 && rtt < 30*time.Second {
						m.UpdateLatency(connIdx, rtt)
					}
				}
				continue
			}
			// Route raw IP packets to ring buffer (blocks when full for backpressure).
			if f.StreamID == 0 && f.Type == FrameData && m.rawPackets != nil {
				if err := m.rawPackets.Push(ctx, f); err != nil {
					return
				}
				continue
			}
			if f.Type == FrameOpen {
				if m.maxStreams > 0 && int(m.streamCount.Load()) >= m.maxStreams {
					m.logger.Warn("max streams reached, rejecting", "stream", f.StreamID, "max", m.maxStreams)
					continue
				}
				s := &Stream{
					ID:           f.StreamID,
					mux:          m,
					recv:         make(chan []byte, 1024),
					done:         make(chan struct{}),
					assignedConn: m.selectConn(),
				}
				m.streamCount.Add(1)
				m.streams.Store(f.StreamID, s)

				if m.acceptedStreams != nil {
					select {
					case m.acceptedStreams <- s:
					default:
						m.logger.Warn("accepted streams buffer full")
					}
				}
				continue
			}
			m.dispatch(f)
		case <-ctx.Done():
			return
		}
	}
}

// MaxFramePayload limits the payload per MUX frame to stay within safe
// DTLS record sizes for TURN relay transport. VK TURN relays use UDP
// internally between peers, so frames must fit within path MTU (~1400)
// minus DTLS overhead (~41 bytes) and MUX header (13 bytes).
const MaxFramePayload = 1200

// Write sends data on the stream, chunking into multiple frames if needed.
func (s *Stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, errors.New("stream closed")
	}
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > MaxFramePayload {
			chunk = p[:MaxFramePayload]
		}
		err := s.mux.sendFrameOn(s.assignedConn, &Frame{
			StreamID: s.ID,
			Type:     FrameData,
			Sequence: s.mux.NextSeq(),
			Length:   uint32(len(chunk)),
			Payload:  chunk,
		})
		if err != nil {
			if total > 0 {
				return total, err
			}
			return 0, err
		}
		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}

// Read receives data from the stream.
func (s *Stream) Read(p []byte) (int, error) {
	// Return leftover data from a previous Read first.
	if len(s.leftover) > 0 {
		n := copy(p, s.leftover)
		s.leftover = s.leftover[n:]
		return n, nil
	}

	data, ok := <-s.recv
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, data)
	if n < len(data) {
		s.leftover = data[n:]
	}
	return n, nil
}

// Close sends a close frame and removes the stream.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.done)
	s.mux.streams.Delete(s.ID)
	s.mux.streamCount.Add(-1)
	return s.mux.sendFrameOn(s.assignedConn, &Frame{
		StreamID: s.ID,
		Type:     FrameClose,
		Sequence: s.mux.NextSeq(),
	})
}
