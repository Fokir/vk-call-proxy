package mux

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ErrMuxClosed is returned when the mux has been closed.
var ErrMuxClosed = errors.New("mux closed")

// pingPayloadSize is the size of the Ping/Pong payload: 8 bytes timestamp + 4 bytes connIdx.
const pingPayloadSize = 12

// stripSeqSize is the size of the per-stream sequence prefix prepended to striped frame payloads.
const stripSeqSize = 4 // 4-byte per-stream sequence prefix in payload

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
	bytesRecv atomic.Int64
	errors    atomic.Int64
	lastUsed  atomic.Int64 // unix nano
	prevSent  atomic.Int64 // bytesSent snapshot at last throughput measurement
	createdAt atomic.Int64 // unix nano, set once at AddConn time
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
	idleTimeoutNs  atomic.Int64  // nanoseconds, 0 = no idle timeout

	rawPackets   *RawRingBuffer // if set, StreamID=0 FrameData is routed here by DispatchLoop
	rawEvictions atomic.Int64   // total evicted raw packets (diagnostic counter)

	acceptedStreams   chan *Stream // DispatchLoop pushes FrameOpen streams here
	closeAcceptOnce  sync.Once

	connDied     chan int        // index of dead connection, buffered
	reconnecting atomic.Int32   // active reconnect count; prevents premature allDead close

	lastPongAt atomic.Int64 // unix nano of last received Pong on any connection

	maxStreams   int          // 0 = unlimited
	streamCount atomic.Int32 // current active stream count

	disableStriping bool       // if true, streams are pinned to one conn even with >1 conn
	adaptiveStripe  atomic.Bool // adaptive: currently striping?
	prevThroughput  atomic.Int64 // bytes/sec before striping was enabled (for comparison)
}

// writeReq is a frame pushed to a connection's async write channel.
type writeReq struct {
	data []byte
	wg   *sync.WaitGroup // decremented after write completes (may be nil)
}

type muxConn struct {
	conn       io.ReadWriteCloser
	index      int        // connection index within Mux.conns
	stats      connStats
	mu         sync.Mutex // serializes writes
	wrrCurrent int64      // smooth weighted round-robin current weight (protected by Mux.mu)
	rtxRing    retransmitRing // retransmit ring for optimistic retransmission
	writeCh    chan writeReq  // async write channel for striped frames
	writeDone  chan struct{}  // closed when writeLoop exits
}

// New creates a multiplexer over the given connections.
// Can be called with zero connections; use AddConn to add them later.
func New(logger *slog.Logger, conns ...io.ReadWriteCloser) *Mux {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mux{
		logger:          logger,
		inFrames:        make(chan *Frame, 256),
		ctx:             ctx,
		cancel:          cancel,
		allDead:         make(chan struct{}),
		connDied:        make(chan int, 32),
		disableStriping: os.Getenv("ENABLE_STRIPING") != "1",
	}
	for i, c := range conns {
		mc := &muxConn{
			conn:      c,
			index:     i,
			writeCh:   make(chan writeReq, 256),
			writeDone: make(chan struct{}),
		}
		mc.stats.lastUsed.Store(time.Now().UnixNano())
		m.conns = append(m.conns, mc)
	}
	// Start reader and writer goroutines for each connection.
	for i, mc := range m.conns {
		m.activeReaders.Add(1)
		go m.writeLoop(mc)
		go m.readLoop(i, mc)
	}
	return m
}

// SetIdleTimeout configures a read deadline on connections.
// If no data arrives within the timeout, the readLoop closes.
// The peer's ping loop (default 30s) prevents false triggers.
func (m *Mux) SetIdleTimeout(d time.Duration) {
	m.idleTimeoutNs.Store(int64(d))
}

// SetMaxStreams sets the maximum number of concurrent streams.
// 0 means unlimited (default). Must be called before DispatchLoop.
func (m *Mux) SetMaxStreams(n int) {
	m.maxStreams = n
}

// EnableStriping forces striping on (overrides adaptive logic).
// By default striping is adaptive — auto-enabled when throughput saturates a single conn.
// Use ENABLE_STRIPING=1 env var for the same effect.
func (m *Mux) EnableStriping() {
	m.disableStriping = false
	m.adaptiveStripe.Store(true)
}

// shouldStripe returns true if new streams should use striping.
// Logic: ENABLE_STRIPING=1 forces on, otherwise adaptive decision.
func (m *Mux) shouldStripe() bool {
	if m.TotalConns() <= 1 {
		return false
	}
	if !m.disableStriping {
		return true // explicitly enabled via env or EnableStriping()
	}
	return m.adaptiveStripe.Load() // adaptive decision
}

// Adaptive striping thresholds.
const (
	// If best conn throughput exceeds this, enable striping for new streams.
	// Set high to prevent auto-enabling over TCP TURN relays where striping
	// causes reorder timeouts due to TCP congestion jitter between connections.
	// Only ENABLE_STRIPING=1 forces it on. Threshold would need to be ~500 KB/s+
	// to work with direct (non-TURN) connections.
	stripeSaturationThreshold = 500_000 // bytes/sec

	// If total throughput drops below this fraction of pre-striping throughput, disable.
	stripeDegradationRatio = 0.7 // 70% — striping making things worse

	// How often to re-evaluate adaptive striping.
	stripeEvalInterval = 10 * time.Second
)

// StartAdaptiveStriping monitors throughput and auto-enables/disables striping.
// Call in a goroutine. Stops when ctx is cancelled.
func (m *Mux) StartAdaptiveStriping(ctx context.Context) {
	ticker := time.NewTicker(stripeEvalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.evalAdaptiveStriping()
		}
	}
}

func (m *Mux) evalAdaptiveStriping() {
	if !m.disableStriping {
		return // striping forced on, skip adaptive
	}
	if m.TotalConns() <= 1 {
		return
	}

	// Measure throughput per conn since last eval
	m.mu.Lock()
	conns := make([]*muxConn, len(m.conns))
	copy(conns, m.conns)
	m.mu.Unlock()

	var totalBps int64
	var bestBps int64
	for _, mc := range conns {
		if mc == nil {
			continue
		}
		sent := mc.stats.bytesSent.Load()
		prev := mc.stats.prevSent.Swap(sent)
		bps := (sent - prev) * int64(time.Second) / int64(stripeEvalInterval)
		if bps < 0 {
			bps = 0
		}
		totalBps += bps
		if bps > bestBps {
			bestBps = bps
		}
	}

	wasStriping := m.adaptiveStripe.Load()

	if !wasStriping {
		// Consider enabling: is best conn saturated?
		if bestBps > stripeSaturationThreshold {
			m.prevThroughput.Store(totalBps)
			m.adaptiveStripe.Store(true)
			m.logger.Info("adaptive striping: ENABLED",
				"best_conn_bps", bestBps,
				"total_bps", totalBps,
				"threshold", stripeSaturationThreshold)
		}
	} else {
		// Consider disabling: did throughput degrade?
		prevBps := m.prevThroughput.Load()
		if prevBps > 0 && totalBps < int64(float64(prevBps)*stripeDegradationRatio) {
			m.adaptiveStripe.Store(false)
			m.logger.Info("adaptive striping: DISABLED (degradation)",
				"total_bps", totalBps,
				"prev_bps", prevBps,
				"ratio", float64(totalBps)/float64(prevBps))
		} else if totalBps > 0 {
			// Update baseline for next comparison
			m.prevThroughput.Store(totalBps)
		}
	}
}

// readLoop reads frames from a single underlying connection.
// DTLS is message-oriented, so we wrap the connection in a bufio.Reader
// to provide stream semantics for ReadFrame's io.ReadFull calls.
func (m *Mux) readLoop(idx int, mc *muxConn) {
	defer func() {
		mc.conn.Close()    // close DTLS connection immediately to free resources
		close(mc.writeCh)  // stop writeLoop (drains remaining frames)
		<-mc.writeDone     // wait for writeLoop to finish
		m.logger.Info("connection stats at death",
			"conn", idx,
			"bytes_sent", mc.stats.bytesSent.Load(),
			"errors", mc.stats.errors.Load(),
		)
		m.RemoveConn(idx)  // retransmit buffered frames + check reorder gaps

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
		if ns := m.idleTimeoutNs.Load(); ns > 0 {
			if d, ok := mc.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
				d.SetReadDeadline(time.Now().Add(time.Duration(ns)))
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
		m.logger.Debug("readLoop frame",
			"conn", idx,
			"stream", f.StreamID,
			"type", f.Type,
			"payload", len(f.Payload),
			"seq", f.Sequence,
		)
		f.connIdx = idx
		mc.stats.bytesRecv.Add(int64(headerSize + len(f.Payload)))
		mc.stats.lastUsed.Store(time.Now().UnixNano())
		if !m.trySendInFrame(f) {
			return
		}
	}
}

// trySendInFrame sends a frame to the inFrames channel with panic recovery
// for the AddConn/Close race where inFrames may be closed while a readLoop
// from a newly added connection tries to send.
func (m *Mux) trySendInFrame(f *Frame) (sent bool) {
	defer func() {
		if r := recover(); r != nil {
			sent = false
		}
	}()
	select {
	case m.inFrames <- f:
		return true
	case <-m.ctx.Done():
		return false
	}
}

// writeLoop drains the async write channel for a connection.
// Batches multiple frames into a single TCP write to reduce syscall
// overhead and avoid Nagle-like delays on small writes.
func (m *Mux) writeLoop(mc *muxConn) {
	defer close(mc.writeDone)
	var batch []writeReq
	for {
		// Block on first frame
		req, ok := <-mc.writeCh
		if !ok {
			return
		}
		batch = append(batch[:0], req)

		// Drain all immediately available frames (non-blocking).
		// Cap total batch size to stay within pion/dtls inboundBufferSize (8192).
		// Each batch becomes one DTLS record; DTLS adds ~90 bytes overhead
		// (header + CID + AEAD). Oversized records cause "short buffer" on receiver.
		const maxBatchBytes = 4 * 1024
		totalLen := len(req.data)
	drain:
		for len(batch) < 64 && totalLen < maxBatchBytes {
			select {
			case r, ok := <-mc.writeCh:
				if !ok {
					break drain
				}
				batch = append(batch, r)
				totalLen += len(r.data)
			default:
				break drain
			}
		}

		// Coalesce into single buffer for one TCP write.
		buf := make([]byte, 0, totalLen)
		for _, r := range batch {
			buf = append(buf, r.data...)
		}

		m.logger.Debug("writeLoop batch",
			"conn", mc.index,
			"frames", len(batch),
			"bytes", totalLen,
			"ch_len", len(mc.writeCh),
		)

		mc.mu.Lock()
		if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			d.SetWriteDeadline(time.Now().Add(5 * time.Second))
		}
		_, err := mc.conn.Write(buf)
		if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			d.SetWriteDeadline(time.Time{})
		}
		mc.mu.Unlock()

		// Signal all waiters
		for _, r := range batch {
			if r.wg != nil {
				r.wg.Done()
			}
		}
		if err != nil {
			m.logger.Debug("writeLoop batch error",
				"conn", mc.index,
				"frames", len(batch),
				"bytes", totalLen,
				"err", err,
			)
			mc.stats.errors.Add(1)
			continue
		}
		mc.stats.bytesSent.Add(int64(totalLen))
		mc.stats.lastUsed.Store(time.Now().UnixNano())
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

	// Phase 1: find minimum latency among live conns for threshold filter.
	var minLat int64
	for _, mc := range m.conns {
		if mc == nil {
			continue
		}
		lat := mc.stats.latency.Load()
		if lat == 0 {
			lat = 1_000_000 // 1ms default
		}
		if minLat == 0 || lat < minLat {
			minLat = lat
		}
	}
	threshold := minLat * 3

	// Phase 2: Build eligible set (latency <= 3x minimum).
	type candidate struct {
		mc  *muxConn
		lat int64
	}
	var eligible []candidate
	for _, mc := range m.conns {
		if mc == nil {
			continue
		}
		lat := mc.stats.latency.Load()
		if lat == 0 {
			lat = 1_000_000
		}
		if lat <= threshold {
			eligible = append(eligible, candidate{mc, lat})
		}
	}
	// Fallback: if filter excluded all, use all live conns
	if len(eligible) == 0 {
		for _, mc := range m.conns {
			if mc == nil {
				continue
			}
			lat := mc.stats.latency.Load()
			if lat == 0 {
				lat = 1_000_000
			}
			eligible = append(eligible, candidate{mc, lat})
		}
	}

	// Phase 3: SWRR over eligible conns.
	var best *muxConn
	bestCW := int64(math.MinInt64)
	var totalWeight int64

	for _, c := range eligible {
		errs := c.mc.stats.errors.Load()
		weight := 1_000_000_000/c.lat - errs*10
		if weight < 1 {
			weight = 1
		}
		c.mc.wrrCurrent += weight
		totalWeight += weight
		if c.mc.wrrCurrent > bestCW {
			bestCW = c.mc.wrrCurrent
			best = c.mc
		}
	}

	if best != nil {
		best.wrrCurrent -= totalWeight
	}
	return best
}

// SendFrame writes a frame using the SWRR-selected connection.
// Use this for non-stream frames (pings, control).
func (m *Mux) SendFrame(f *Frame) error {
	return m.sendFrameOn(m.selectConn(), f)
}

// SendRawPacket writes a raw IP packet frame with a short write deadline
// and no fallback. If the selected connection is congested, the packet is
// dropped silently — this prevents TCP bidirectional congestion on TURN
// relays from blocking ALL connections during heavy upload (e.g. speed tests).
// TCP retransmits lost packets; UDP is best-effort by nature.
func (m *Mux) SendRawPacket(f *Frame) error {
	mc := m.selectConn()
	if mc == nil {
		return errors.New("no connections available")
	}

	data, err := f.MarshalBinary()
	if err != nil {
		return err
	}

	const rawWriteDeadline = 200 * time.Millisecond

	mc.mu.Lock()
	if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		d.SetWriteDeadline(time.Now().Add(rawWriteDeadline))
	}
	_, err = mc.conn.Write(data)
	if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		d.SetWriteDeadline(time.Time{})
	}
	mc.mu.Unlock()

	if err != nil {
		// Don't fallback, don't count as connection error — deadline timeouts
		// during congestion are expected and should not degrade SWRR weight.
		return err
	}
	mc.stats.bytesSent.Add(int64(len(data)))
	mc.stats.lastUsed.Store(time.Now().UnixNano())
	return nil
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
		m.logger.Debug("sendFrameOn write failed",
			"conn", mc.index,
			"stream", f.StreamID,
			"type", f.Type,
			"payload", len(f.Payload),
			"err", err,
		)
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
	mc := &muxConn{
		conn:      conn,
		writeCh:   make(chan writeReq, 256),
		writeDone: make(chan struct{}),
	}
	now := time.Now().UnixNano()
	mc.stats.lastUsed.Store(now)
	mc.stats.createdAt.Store(now)

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
	mc.index = idx
	m.mu.Unlock()

	m.activeReaders.Add(1)
	go m.writeLoop(mc)
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

// ConnStat holds exported per-connection statistics.
type ConnStat struct {
	Index     int
	BytesSent int64
	BytesRecv int64
	LatencyNs int64
	Alive     bool
}

// ConnStats returns a snapshot of per-connection statistics.
func (m *Mux) ConnStats() []ConnStat {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats := make([]ConnStat, len(m.conns))
	for i, mc := range m.conns {
		if mc == nil {
			continue
		}
		stats[i] = ConnStat{
			Index:     i,
			BytesSent: mc.stats.bytesSent.Load(),
			BytesRecv: mc.stats.bytesRecv.Load(),
			LatencyNs: mc.stats.latency.Load(),
			Alive:     mc.conn != nil,
		}
	}
	return stats
}

// CloseOldestConn closes the oldest connection if its age exceeds maxAge
// and at least minAlive connections would remain alive. Returns true if
// a connection was closed (it will trigger ConnDied asynchronously).
func (m *Mux) CloseOldestConn(maxAge time.Duration, minAlive int) bool {
	now := time.Now().UnixNano()

	m.mu.Lock()
	var oldest *muxConn
	oldestAge := int64(0)
	alive := 0
	for _, mc := range m.conns {
		if mc == nil {
			continue
		}
		alive++
		age := now - mc.stats.createdAt.Load()
		if age > oldestAge {
			oldestAge = age
			oldest = mc
		}
	}
	m.mu.Unlock()

	if oldest == nil || alive <= minAlive || oldestAge < int64(maxAge) {
		return false
	}

	m.logger.Info("closing oldest connection for rotation",
		"index", oldest.index,
		"age", time.Duration(oldestAge).Round(time.Second),
		"alive", alive,
	)
	oldest.conn.Close()
	return true
}

// RemoveConn sets the connection at the given index to nil.
// The slot is reused by subsequent AddConn calls.
// When a connection dies, its retransmit ring is drained and
// frames are resent via surviving connections (best-effort).
func (m *Mux) RemoveConn(idx int) {
	m.mu.Lock()
	var deadConn *muxConn
	if idx >= 0 && idx < len(m.conns) {
		deadConn = m.conns[idx]
		m.conns[idx] = nil
	}
	m.mu.Unlock()

	if deadConn == nil {
		return
	}

	// Optimistic retransmit: resend buffered frames via surviving conns
	frames := deadConn.rtxRing.Drain()
	for _, data := range frames {
		mc := m.selectConn()
		if mc == nil {
			break // no surviving conns
		}
		mc.mu.Lock()
		if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			d.SetWriteDeadline(time.Now().Add(5 * time.Second))
		}
		mc.conn.Write(data) // best-effort, ignore errors
		if d, ok := mc.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			d.SetWriteDeadline(time.Time{})
		}
		mc.mu.Unlock()
	}

	// Check all streams' reorder buffers for stale gaps
	m.streams.Range(func(key, val any) bool {
		s := val.(*Stream)
		if s.reorder != nil && s.reorder.Len() > 0 {
			if s.reorder.GapTimedOut(reorderGapTimeout) {
				m.logger.Warn("conn died, reorder gap timed out", "stream", s.ID)
				m.closeStream(s)
			}
		}
		return true
	})
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
	conns := make([]*muxConn, len(m.conns))
	copy(conns, m.conns)
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

	// Striping fields (active when stream created with >1 conn)
	striping     bool
	nextStripSeq atomic.Uint32  // sender: per-stream sequence counter
	reorder      *reorderBuffer // receiver: reorder buffer (nil if !striping)
	closePending atomic.Bool    // receiver: FrameClose deferred until reorder drains
	inflight     sync.WaitGroup // sender: tracks in-flight async writes
}

// OpenStream creates a new logical stream and sends an open frame to the peer.
// The stream is pinned to a connection chosen via SWRR for write ordering.
func (m *Mux) OpenStream(id uint32) (*Stream, error) {
	assigned := m.selectConn()
	striping := m.shouldStripe()
	s := &Stream{
		ID:           id,
		mux:          m,
		recv:         make(chan []byte, 1024),
		done:         make(chan struct{}),
		assignedConn: assigned,
		striping:     striping,
	}
	if striping {
		s.reorder = newReorderBuffer()
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
				striping := m.shouldStripe()
				s := &Stream{
					ID:       f.StreamID,
					mux:      m,
					recv:     make(chan []byte, 1024),
					done:     make(chan struct{}),
					striping: striping,
				}
				if striping {
					s.reorder = newReorderBuffer()
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
		if s.striping && s.reorder != nil {
			m.dispatchStriped(s, f)
		} else {
			select {
			case s.recv <- f.Payload:
			case <-s.done:
			}
		}
	case FrameClose:
		if s.striping && s.reorder != nil && s.reorder.Len() > 0 {
			// Defer close until reorder buffer drains
			s.closePending.Store(true)
			return
		}
		m.closeStream(s)
	}
}

func (m *Mux) dispatchStriped(s *Stream, f *Frame) {
	if len(f.Payload) < stripSeqSize {
		return // malformed
	}
	stripSeq := binary.BigEndian.Uint32(f.Payload[0:stripSeqSize])
	data := make([]byte, len(f.Payload)-stripSeqSize)
	copy(data, f.Payload[stripSeqSize:])

	flushed := s.reorder.Insert(stripSeq, data)
	for _, d := range flushed {
		select {
		case s.recv <- d:
		case <-s.done:
			return
		}
	}

	// Check limits
	if s.reorder.Overflowed() || s.reorder.GapTimedOut(reorderGapTimeout) {
		m.logger.Warn("reorder buffer limit reached, closing stream",
			"stream", s.ID,
			"buffered", s.reorder.Len(),
			"timedOut", s.reorder.GapTimedOut(reorderGapTimeout))
		m.closeStream(s)
		return
	}

	// If close was pending and buffer is now empty, execute deferred close
	if s.closePending.Load() && s.reorder.Len() == 0 {
		m.closeStream(s)
	}
}

func (m *Mux) closeStream(s *Stream) {
	if s.closed.Swap(true) {
		return // already closed — prevent double-close panic on s.recv
	}
	close(s.done)
	m.streams.Delete(s.ID)
	m.streamCount.Add(-1)
	// Drain any remaining reorder buffer before closing recv
	if s.reorder != nil {
		for _, d := range s.reorder.FlushAll() {
			select {
			case s.recv <- d:
			case <-time.After(100 * time.Millisecond):
				// recv channel full or blocked, accept data loss
			}
		}
	}
	close(s.recv)
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
	// pendingFrames buffers frames that arrive before their stream's FrameOpen.
	// With striping, FrameOpen and FrameData may travel on different connections
	// and arrive out of order in DispatchLoop.
	pendingFrames := make(map[uint32][]*Frame)
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
			// Route raw IP packets to ring buffer (evicts oldest when full).
			if f.StreamID == 0 && f.Type == FrameData && m.rawPackets != nil {
				if evicted := m.rawPackets.Push(f); evicted > 0 {
					m.rawEvictions.Add(1)
					if n := m.rawEvictions.Load(); n == 1 || n%1000 == 0 {
						m.logger.Warn("raw packet buffer full, evicting old frames", "total_evictions", n)
					}
				}
				continue
			}
			if f.Type == FrameOpen {
				if m.maxStreams > 0 && int(m.streamCount.Load()) >= m.maxStreams {
					m.logger.Warn("max streams reached, rejecting", "stream", f.StreamID, "max", m.maxStreams)
					continue
				}
				striping := m.shouldStripe()
				s := &Stream{
					ID:           f.StreamID,
					mux:          m,
					recv:         make(chan []byte, 1024),
					done:         make(chan struct{}),
					assignedConn: m.selectConn(),
					striping:     striping,
				}
				if striping {
					s.reorder = newReorderBuffer()
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
				// Replay any frames that arrived before this FrameOpen.
				if pending, ok := pendingFrames[f.StreamID]; ok {
					for _, pf := range pending {
						m.dispatch(pf)
					}
					delete(pendingFrames, f.StreamID)
				}
				continue
			}
			// Buffer frames for streams not yet opened (striping race:
			// FrameOpen and FrameData travel on different connections).
			if f.StreamID != 0 && f.Type == FrameData {
				if _, exists := m.streams.Load(f.StreamID); !exists {
					pendingFrames[f.StreamID] = append(pendingFrames[f.StreamID], f)
					continue
				}
			}
			m.dispatch(f)
		case <-ctx.Done():
			return
		}
	}
}

// DefaultFramePayload is the safe payload size for UDP TURN relays
// (path MTU ~1400 minus DTLS overhead ~41 bytes and MUX header 13 bytes).
const DefaultFramePayload = 1200

// MaxFramePayload is the active payload limit per MUX frame.
// Configurable via MUX_FRAME_SIZE env var (bytes). For TCP TURN there is no
// MTU constraint so larger frames (e.g. 65535) reduce framing overhead.
var MaxFramePayload = func() int {
	if v := os.Getenv("MUX_FRAME_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxPayload {
			return n
		}
	}
	return DefaultFramePayload
}()

// trySendWriteCh attempts a non-blocking send to mc.writeCh.
// Returns false if the channel is full or closed (recovers from panic on closed channel).
func (s *Stream) trySendWriteCh(mc *muxConn, data []byte) (sent bool) {
	defer func() {
		if r := recover(); r != nil {
			sent = false
		}
	}()
	select {
	case mc.writeCh <- writeReq{data: data, wg: &s.inflight}:
		return true
	default:
		return false
	}
}

// Write sends data on the stream, chunking into multiple frames if needed.
func (s *Stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, errors.New("stream closed")
	}
	maxChunk := MaxFramePayload
	if s.striping {
		maxChunk = MaxFramePayload - stripSeqSize
	}
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxChunk {
			chunk = p[:maxChunk]
		}

		var payload []byte
		if s.striping {
			payload = make([]byte, stripSeqSize+len(chunk))
			binary.BigEndian.PutUint32(payload[0:stripSeqSize], s.nextStripSeq.Add(1)-1)
			copy(payload[stripSeqSize:], chunk)
		} else {
			payload = chunk
		}

		f := &Frame{
			StreamID: s.ID,
			Type:     FrameData,
			Sequence: s.mux.NextSeq(),
			Length:   uint32(len(payload)),
			Payload:  payload,
		}

		var mc *muxConn
		if s.striping {
			mc = s.mux.selectConn() // round-robin across all conns
		} else {
			mc = s.assignedConn // pinned to one conn for ordering
		}
		if mc == nil {
			if total > 0 {
				return total, errors.New("no connections available")
			}
			return 0, errors.New("no connections available")
		}

		if s.striping {
			// Async write: push marshaled frame to conn's write channel.
			data, err := f.MarshalBinary()
			if err != nil {
				if total > 0 {
					return total, err
				}
				return 0, err
			}
			mc.rtxRing.mu.Lock()
			mc.rtxRing.Push(data)
			mc.rtxRing.mu.Unlock()
			s.inflight.Add(1)
			if !s.trySendWriteCh(mc, data) {
				// Channel full — try other conns to avoid head-of-line blocking.
				// With striping, blocking on one slow conn starves ALL conns.
				sent := false
				s.mux.mu.Lock()
				for _, alt := range s.mux.conns {
					if alt != nil && alt != mc {
						if s.trySendWriteCh(alt, data) {
							sent = true
							break
						}
					}
				}
				s.mux.mu.Unlock()
				if !sent {
					// All channels full — sync write as last resort.
					s.mux.sendFrameOn(mc, f)
					s.inflight.Done()
				}
				// If sent via alt conn, trySendWriteCh already
				// passed the wg to writeReq — don't double-Done.
			}
		} else {
			err := s.mux.sendFrameOn(mc, f)
			if err != nil {
				if total > 0 {
					return total, err
				}
				return 0, err
			}
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
// For striped streams, waits for all async writes to complete first.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.striping {
		s.inflight.Wait() // flush all pending async writes
	}
	close(s.done)
	s.mux.streams.Delete(s.ID)
	s.mux.streamCount.Add(-1)
	// Striped: any conn is fine (data already flushed).
	// Non-striped: use assigned conn to preserve ordering with data frames.
	mc := s.assignedConn
	if s.striping {
		mc = nil // selectConn picks fastest
	}
	return s.mux.sendFrameOn(mc, &Frame{
		StreamID: s.ID,
		Type:     FrameClose,
		Sequence: s.mux.NextSeq(),
	})
}
