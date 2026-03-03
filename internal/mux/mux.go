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

// ErrMuxClosed is returned when the mux has been closed.
var ErrMuxClosed = errors.New("mux closed")

// StartPingLoop sends periodic ping frames on EVERY underlying connection
// to keep TURN allocations alive. Sending on all connections prevents idle
// connections from timing out (TURN allocations have a 5-minute TTL,
// server idle timeout is typically 2 min). Call in a goroutine.
func (m *Mux) StartPingLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			f := &Frame{StreamID: 0, Type: FramePing, Sequence: m.NextSeq()}
			data, err := f.MarshalBinary()
			if err != nil {
				continue
			}
			m.mu.Lock()
			conns := make([]*muxConn, len(m.conns))
			copy(conns, m.conns)
			m.mu.Unlock()

			for _, mc := range conns {
				mc.mu.Lock()
				mc.conn.Write(data)
				mc.mu.Unlock()
			}
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

	rawPackets   chan *Frame // if set, StreamID=0 FrameData is routed here by DispatchLoop
	closeRawOnce sync.Once

	acceptedStreams   chan *Stream // DispatchLoop pushes FrameOpen streams here
	closeAcceptOnce  sync.Once
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
		allDead:  make(chan struct{}),
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

// readLoop reads frames from a single underlying connection.
// DTLS is message-oriented, so we wrap the connection in a bufio.Reader
// to provide stream semantics for ReadFrame's io.ReadFull calls.
func (m *Mux) readLoop(idx int, mc *muxConn) {
	defer func() {
		if m.activeReaders.Add(-1) == 0 {
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

// EnableRawPackets creates a dedicated channel for raw IP packets
// (StreamID=0, FrameData). DispatchLoop routes matching frames here
// instead of dispatching them as stream data. Must be called before
// DispatchLoop is started.
func (m *Mux) EnableRawPackets(bufSize int) {
	m.rawPackets = make(chan *Frame, bufSize)
}

// RawPackets returns the channel for raw IP packets, or nil if
// EnableRawPackets was not called.
func (m *Mux) RawPackets() <-chan *Frame {
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

	m.activeReaders.Add(1)
	go m.readLoop(idx, mc)
}

// Dead returns a channel that is closed when all readLoop goroutines
// have exited (all underlying connections are dead).
func (m *Mux) Dead() <-chan struct{} {
	return m.allDead
}

// Close shuts down the multiplexer and all underlying connections.
func (m *Mux) Close() error {
	m.cancel()
	m.closeInFrames.Do(func() { close(m.inFrames) })
	if m.rawPackets != nil {
		m.closeRawOnce.Do(func() { close(m.rawPackets) })
	}
	if m.acceptedStreams != nil {
		m.closeAcceptOnce.Do(func() { close(m.acceptedStreams) })
	}
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
	ID       uint32
	mux      *Mux
	recv     chan []byte
	closed   atomic.Bool
	leftover []byte // unread remainder from previous Read
}

// OpenStream creates a new logical stream and sends an open frame to the peer.
func (m *Mux) OpenStream(id uint32) (*Stream, error) {
	s := &Stream{
		ID:   id,
		mux:  m,
		recv: make(chan []byte, 1024),
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
		case f, ok := <-m.inFrames:
			if !ok {
				return nil, ErrMuxClosed
			}
			if f.Type == FrameOpen {
				s := &Stream{
					ID:   f.StreamID,
					mux:  m,
					recv: make(chan []byte, 1024),
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
// If EnableRawPackets was called, StreamID=0 FrameData frames are
// forwarded to the RawPackets channel instead of being dispatched.
// If EnableStreamAccept was called, new streams from FrameOpen are
// pushed to AcceptedStreams channel.
func (m *Mux) DispatchLoop(ctx context.Context) {
	defer func() {
		if m.rawPackets != nil {
			m.closeRawOnce.Do(func() { close(m.rawPackets) })
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
			// Handle Ping/Pong keepalive.
			if f.Type == FramePing {
				_ = m.SendFrame(&Frame{StreamID: 0, Type: FramePong, Sequence: m.NextSeq()})
				continue
			}
			if f.Type == FramePong {
				continue
			}
			// Route raw IP packets to dedicated channel.
			if f.StreamID == 0 && f.Type == FrameData && m.rawPackets != nil {
				select {
				case m.rawPackets <- f:
				default:
					m.logger.Warn("raw packet buffer full, dropping frame")
				}
				continue
			}
			if f.Type == FrameOpen {
				s := &Stream{
					ID:   f.StreamID,
					mux:  m,
					recv: make(chan []byte, 1024),
				}
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
		err := s.mux.SendFrame(&Frame{
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
	s.mux.streams.Delete(s.ID)
	return s.mux.SendFrame(&Frame{
		StreamID: s.ID,
		Type:     FrameClose,
		Sequence: s.mux.NextSeq(),
	})
}
