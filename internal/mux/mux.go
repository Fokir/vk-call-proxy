package mux

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// StartPingLoop sends periodic ping frames through the mux to keep
// TURN allocations alive. TURN allocations have a 5-minute TTL,
// so we ping every 30 seconds by default. Call in a goroutine.
func (m *Mux) StartPingLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_ = m.SendFrame(&Frame{
				StreamID: 0,
				Type:     FramePing,
				Sequence: m.NextSeq(),
			})
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

	inFrames chan *Frame
	ctx      context.Context
	cancel   context.CancelFunc
}

type muxConn struct {
	conn  io.ReadWriteCloser
	stats connStats
	mu    sync.Mutex // serializes writes
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
	}
	for _, c := range conns {
		mc := &muxConn{conn: c}
		mc.stats.lastUsed.Store(time.Now().UnixNano())
		m.conns = append(m.conns, mc)
	}
	// Start reader goroutines for each connection.
	for i, mc := range m.conns {
		go m.readLoop(i, mc)
	}
	return m
}

// readLoop reads frames from a single underlying connection.
// DTLS is message-oriented, so we wrap the connection in a bufio.Reader
// to provide stream semantics for ReadFrame's io.ReadFull calls.
func (m *Mux) readLoop(idx int, mc *muxConn) {
	br := bufio.NewReaderSize(mc.conn, 16384)
	for {
		f, err := ReadFrame(br)
		if err != nil {
			if m.ctx.Err() != nil {
				return
			}
			mc.stats.errors.Add(1)
			m.logger.Warn("read error on connection", "index", idx, "err", err)
			return
		}
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
func (m *Mux) selectConn() *muxConn {
	m.mu.Lock()
	conns := make([]*muxConn, len(m.conns))
	copy(conns, m.conns)
	m.mu.Unlock()

	if len(conns) == 0 {
		return nil
	}
	if len(conns) == 1 {
		return conns[0]
	}

	var best *muxConn
	bestScore := int64(-1)

	for _, mc := range conns {
		lat := mc.stats.latency.Load()
		if lat == 0 {
			lat = 1_000_000 // 1ms default
		}
		errs := mc.stats.errors.Load()
		// Score: lower latency and fewer errors = higher score.
		// Inverse latency scaled, penalty for errors.
		score := (1_000_000_000 / lat) - errs*100
		if best == nil || score > bestScore {
			best = mc
			bestScore = score
		}
	}
	return best
}

// SendFrame writes a frame to the best available connection.
func (m *Mux) SendFrame(f *Frame) error {
	mc := m.selectConn()
	if mc == nil {
		return errors.New("no connections available")
	}

	data, err := f.MarshalBinary()
	if err != nil {
		return err
	}

	mc.mu.Lock()
	_, err = mc.conn.Write(data)
	mc.mu.Unlock()

	if err != nil {
		mc.stats.errors.Add(1)
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
// for a given session.
func (m *Mux) AddConn(conn io.ReadWriteCloser) {
	mc := &muxConn{conn: conn}
	mc.stats.lastUsed.Store(time.Now().UnixNano())

	m.mu.Lock()
	idx := len(m.conns)
	m.conns = append(m.conns, mc)
	m.mu.Unlock()

	go m.readLoop(idx, mc)
}

// Close shuts down the multiplexer and all underlying connections.
func (m *Mux) Close() error {
	m.cancel()
	m.mu.Lock()
	conns := m.conns
	m.mu.Unlock()
	for _, mc := range conns {
		mc.conn.Close()
	}
	return nil
}

// Stream represents a logical bidirectional stream within the mux.
type Stream struct {
	ID     uint32
	mux    *Mux
	recv   chan []byte
	closed atomic.Bool
}

// OpenStream creates a new logical stream and sends an open frame to the peer.
func (m *Mux) OpenStream(id uint32) (*Stream, error) {
	s := &Stream{
		ID:   id,
		mux:  m,
		recv: make(chan []byte, 64),
	}
	m.streams.Store(id, s)

	err := m.SendFrame(&Frame{
		StreamID: id,
		Type:     FrameOpen,
		Sequence: m.NextSeq(),
	})
	if err != nil {
		m.streams.Delete(id)
		return nil, err
	}
	return s, nil
}

// AcceptStream returns a stream when an open frame is received.
func (m *Mux) AcceptStream(ctx context.Context) (*Stream, error) {
	for {
		select {
		case f := <-m.inFrames:
			if f.Type == FrameOpen {
				s := &Stream{
					ID:   f.StreamID,
					mux:  m,
					recv: make(chan []byte, 64),
				}
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
		default:
			m.logger.Warn("stream recv buffer full, dropping frame", "stream", f.StreamID)
		}
	case FrameClose:
		s.closed.Store(true)
		close(s.recv)
		m.streams.Delete(f.StreamID)
	}
}

// DispatchLoop processes incoming frames and routes them to streams.
// Call this in a goroutine on the client/server side.
func (m *Mux) DispatchLoop(ctx context.Context) {
	for {
		select {
		case f, ok := <-m.inFrames:
			if !ok {
				return
			}
			if f.Type == FrameOpen {
				// Handled by AcceptStream
				s := &Stream{
					ID:   f.StreamID,
					mux:  m,
					recv: make(chan []byte, 64),
				}
				m.streams.Store(f.StreamID, s)
				continue
			}
			m.dispatch(f)
		case <-ctx.Done():
			return
		}
	}
}

// Write sends data on the stream.
func (s *Stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, errors.New("stream closed")
	}
	err := s.mux.SendFrame(&Frame{
		StreamID: s.ID,
		Type:     FrameData,
		Sequence: s.mux.NextSeq(),
		Length:   uint32(len(p)),
		Payload:  p,
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read receives data from the stream.
func (s *Stream) Read(p []byte) (int, error) {
	data, ok := <-s.recv
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, data)
	return n, nil
}

// Close sends a close frame and removes the stream.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.mux.streams.Delete(s.ID)
	return s.mux.SendFrame(&Frame{
		StreamID: s.ID,
		Type:     FrameClose,
		Sequence: s.mux.NextSeq(),
	})
}
