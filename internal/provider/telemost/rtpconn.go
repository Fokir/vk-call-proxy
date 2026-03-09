package telemost

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// RTPConn wraps publisher video track (write) and subscriber video track (read)
// into a bidirectional io.ReadWriteCloser. VPN data is encoded as VP8 keyframes
// and forwarded by the SFU through its standard RTP media pipeline.
//
// The reliability layer (FEC + ARQ) ensures data delivery over the lossy SFU channel:
// - XOR FEC with interleaved groups recovers single losses per group
// - NACK-based retransmission handles cases FEC can't recover
//
// When multiple video tracks are received (e.g. stale participants in the room),
// RTPConn locks to the first track that delivers valid VPN data and ignores others.

// maxVP8Data is the maximum VPN data per VP8 frame to ensure each frame fits
// in a single RTP packet. RTP MTU ~1200 bytes minus VP8 RTP descriptor (~5 bytes)
// minus VP8 header (16 bytes) minus reliability header (3 bytes) minus chunk header (4 bytes).
const maxVP8Data = 1100

// Adaptive pacing constants (AIMD). The write pace between consecutive VP8
// frames is dynamically adjusted based on the rate of incoming NACKs from
// the remote peer — a high NACK rate signals SFU congestion.
const (
	defaultPace         = 5 * time.Millisecond   // initial pace
	minPace             = 2 * time.Millisecond   // fastest (~500 fps, ~110 KB/s)
	maxPace             = 10 * time.Millisecond  // slowest (~100 fps, ~22 KB/s)
	paceAdjustInterval  = 500 * time.Millisecond // how often we re-evaluate
	paceDecrease        = 500 * time.Microsecond // additive increase (decrease pace)
	paceMultiplier      = 1.5                    // multiplicative decrease (increase pace)
	nackRateThreshold   = 3                      // NACKs per window to trigger back-off
)

// nackCheckInterval is how often we check for expired NACKs to send.
const nackCheckInterval = 15 * time.Millisecond

// Packet types for the reliability layer.
const (
	pktDATA byte = 0x01 // normal data chunk
	pktFEC  byte = 0x02 // XOR FEC parity
	pktNACK byte = 0x03 // request retransmission
	pktRETX byte = 0x04 // retransmitted data chunk
)

type RTPConn struct {
	pubTrack *webrtc.TrackLocalStaticSample // publisher video track (write path)
	obfKey   [32]byte                       // XOR obfuscation key for payload masking
	once     sync.Once
	closeCh  chan struct{}

	lockedMu   sync.Mutex
	lockedSSRC uint32 // SSRC of the track we've locked to (0 = not yet locked)

	seqMu     sync.Mutex
	seqNum    uint16    // write sequence counter (per-Write() call)
	lastWrite time.Time // last VP8 frame write time for pacing

	// Packet-level sequence for the reliability layer (per VP8 frame).
	pktSeqMu   sync.Mutex
	nextPktSeq uint16

	// Reliability components.
	fecEncoder *FECEncoder
	fecDecoder *FECDecoder
	retxBuf    *RetransmitBuffer
	nackTrack  *NACKTracker

	// rawPktCh receives VP8 payloads from HandleTrack (with reliability header).
	rawPktCh chan []byte
	// readCh receives chunkPayloads after reliability processing.
	readCh chan []byte

	// Reassembly state (single-goroutine access from reassemblyLoop).
	reassembly  map[uint16]*reassemblyBuf
	nextReadSeq uint16     // next expected write-sequence for ordered delivery
	nrsMu       sync.Mutex // protects nextReadSeq for cross-goroutine reads
	orderedCh   chan []byte

	// Reset channels for switching to a new peer after pin.
	resetReliabilityCh chan struct{} // signals reliabilityLoop to clear FEC state
	resetReassemblyCh  chan struct{} // signals reassemblyLoop to clear buffers

	// validDataCh is signaled when the first valid VP8 data is decoded after a reset.
	// Used by ConnectPaired to probe whether a pinned candidate is the real peer.
	validDataCh chan struct{}

	// Adaptive pacing state.
	currentPace atomic.Int64 // current write pace in nanoseconds
	nackRecv    atomic.Int64 // NACKs received from peer in current window
	writeCount  atomic.Int64 // VP8 frames written in current window
}

type reassemblyBuf struct {
	chunks [][]byte
	total  int
	got    int
}

// DeriveObfuscationKey computes a 32-byte XOR key from a passphrase.
// If passphrase is empty, a fixed default key is used so that the protocol
// signatures (vpnMagic, pktType headers) are still masked in the VP8 stream.
func DeriveObfuscationKey(passphrase string) [32]byte {
	if passphrase == "" {
		passphrase = "call-vpn-default-obfuscation"
	}
	return sha256.Sum256([]byte(passphrase + "\x00obfuscate"))
}

// DeriveIndexedObfuscationKey derives a per-connection obfuscation key.
// Each connection pair (server[i], client[i]) uses a unique key so that
// VP8 data from other connections in the same conference is rejected by
// extractVP8Data (magic mismatch after deobfuscation).
func DeriveIndexedObfuscationKey(passphrase string, index int) [32]byte {
	if passphrase == "" {
		passphrase = "call-vpn-default-obfuscation"
	}
	return sha256.Sum256([]byte(fmt.Sprintf("%s\x00obfuscate\x00%d", passphrase, index)))
}

// DeriveSessionObfuscationKey derives a session-specific obfuscation key by mixing
// the base key (from token+index) with a random nonce exchanged through Goloom WS.
// This ensures ghost participants from previous sessions can't produce valid VP8 data.
func DeriveSessionObfuscationKey(passphrase string, index int, nonce string) [32]byte {
	if passphrase == "" {
		passphrase = "call-vpn-default-obfuscation"
	}
	return sha256.Sum256([]byte(fmt.Sprintf("%s\x00obfuscate\x00%d\x00%s", passphrase, index, nonce)))
}

// SetObfKey updates the obfuscation key. Used after session nonce exchange.
func (c *RTPConn) SetObfKey(key [32]byte) {
	c.obfKey = key
}

// NewRTPConn creates a bidirectional connection using RTP video tracks.
// obfKey is used to XOR-mask VP8 payload data (hiding protocol signatures).
func NewRTPConn(pubTrack *webrtc.TrackLocalStaticSample, obfKey [32]byte) *RTPConn {
	c := &RTPConn{
		pubTrack:           pubTrack,
		obfKey:             obfKey,
		closeCh:            make(chan struct{}),
		rawPktCh:           make(chan []byte, 256),
		readCh:             make(chan []byte, 256),
		reassembly:         make(map[uint16]*reassemblyBuf),
		orderedCh:          make(chan []byte, 64),
		fecEncoder:         NewFECEncoder(),
		fecDecoder:         NewFECDecoder(),
		retxBuf:            &RetransmitBuffer{},
		nackTrack:          NewNACKTracker(),
		resetReliabilityCh: make(chan struct{}, 1),
		resetReassemblyCh:  make(chan struct{}, 1),
		validDataCh:        make(chan struct{}, 1),
	}
	c.currentPace.Store(int64(defaultPace))
	go c.reliabilityLoop()
	go c.reassemblyLoop()
	go c.nackLoop()
	go c.paceAdjustLoop()
	return c
}

func (c *RTPConn) nextSeq() uint16 {
	c.seqMu.Lock()
	s := c.seqNum
	c.seqNum++
	c.seqMu.Unlock()
	return s
}

func (c *RTPConn) nextPktSeqNum() uint16 {
	c.pktSeqMu.Lock()
	s := c.nextPktSeq
	c.nextPktSeq++
	c.pktSeqMu.Unlock()
	return s
}

// reassemblyTimeout is the maximum time to wait for a missing write-sequence.
// If FEC + repeated NACKs can't recover the data within this window,
// the connection is closed (MUX requires reliable delivery — skipping corrupts framing).
const reassemblyTimeout = 3 * time.Second

// reassemblyLoop reads chunkPayloads from readCh, reassembles multi-chunk
// messages, and delivers them in write-sequence order to orderedCh.
func (c *RTPConn) reassemblyLoop() {
	var stallTimer *time.Timer
	var stallCh <-chan time.Time

	for {
		select {
		case <-c.closeCh:
			if stallTimer != nil {
				stallTimer.Stop()
			}
			return

		case <-c.resetReassemblyCh:
			c.reassembly = make(map[uint16]*reassemblyBuf)
			if stallTimer != nil {
				stallTimer.Stop()
				stallCh = nil
			}
			// Drain stale chunks from readCh.
		drainRead:
			for {
				select {
				case <-c.readCh:
				default:
					break drainRead
				}
			}

		case <-stallCh:
			// Check if the stalled sequence was recovered while we waited.
			rb, exists := c.reassembly[c.nextReadSeq]
			if exists && rb.got >= rb.total {
				stallCh = nil
				c.deliverReady()
				if c.hasBufferedAhead() {
					stallTimer.Reset(reassemblyTimeout)
					stallCh = stallTimer.C
				}
				continue
			}
			// Unrecoverable loss: FEC + NACK retries couldn't recover the data
			// within the timeout. Close the connection — MUX requires reliable
			// delivery, skipping would corrupt framing.
			slog.Error("rtpconn: unrecoverable loss, closing connection",
				"missingWriteSeq", c.nextReadSeq,
				"timeout", reassemblyTimeout)
			c.Close()
			return

		case raw, ok := <-c.readCh:
			if !ok {
				if stallTimer != nil {
					stallTimer.Stop()
				}
				return
			}
			if len(raw) < 4 {
				continue
			}
			seq := binary.BigEndian.Uint16(raw[0:2])
			idx := int(raw[2])
			total := int(raw[3])
			data := raw[4:]

			if total < 1 || idx >= total {
				continue
			}

			buf, exists := c.reassembly[seq]
			if !exists {
				buf = &reassemblyBuf{
					chunks: make([][]byte, total),
					total:  total,
				}
				c.reassembly[seq] = buf
			}
			if buf.chunks[idx] == nil {
				chunk := make([]byte, len(data))
				copy(chunk, data)
				buf.chunks[idx] = chunk
				buf.got++
			}

			c.deliverReady()

			if stallCh == nil && c.hasBufferedAhead() {
				if stallTimer == nil {
					stallTimer = time.NewTimer(reassemblyTimeout)
				} else {
					stallTimer.Reset(reassemblyTimeout)
				}
				stallCh = stallTimer.C
			}
			if stallCh != nil && !c.hasBufferedAhead() {
				stallTimer.Stop()
				stallCh = nil
			}
		}
	}
}

// deliverReady delivers all consecutive completed sequences starting from nextReadSeq.
func (c *RTPConn) deliverReady() {
	for {
		rb, ok := c.reassembly[c.nextReadSeq]
		if !ok || rb.got < rb.total {
			break
		}
		totalLen := 0
		for _, ch := range rb.chunks {
			totalLen += len(ch)
		}
		assembled := make([]byte, 0, totalLen)
		for _, ch := range rb.chunks {
			assembled = append(assembled, ch...)
		}
		delete(c.reassembly, c.nextReadSeq)
		c.incNextReadSeq()

		if len(assembled) == 0 {
			continue
		}

		select {
		case c.orderedCh <- assembled:
		case <-c.closeCh:
			return
		}
	}
}

// hasBufferedAhead returns true if there's any reassembly data with seq > nextReadSeq
// but nextReadSeq itself is missing or incomplete.
func (c *RTPConn) hasBufferedAhead() bool {
	_, currentExists := c.reassembly[c.nextReadSeq]
	if currentExists {
		rb := c.reassembly[c.nextReadSeq]
		if rb.got >= rb.total {
			return false
		}
	}
	for seq := range c.reassembly {
		if seqAfter(seq, c.nextReadSeq) {
			return true
		}
	}
	return false
}

// reliabilityLoop processes raw packets from HandleTrack, handles FEC/NACK,
// and forwards data chunks to readCh for reassembly.
func (c *RTPConn) reliabilityLoop() {
	for {
		select {
		case <-c.closeCh:
			return
		case <-c.resetReliabilityCh:
			c.fecDecoder = NewFECDecoder()
			// Drain stale packets from rawPktCh.
		drainRaw:
			for {
				select {
				case <-c.rawPktCh:
				default:
					break drainRaw
				}
			}
		case raw, ok := <-c.rawPktCh:
			if !ok {
				return
			}
			if len(raw) < 1 {
				continue
			}
			pktType := raw[0]

			switch pktType {
			case pktDATA, pktRETX:
				c.handleDataPacket(raw)
			case pktFEC:
				c.handleFECPacket(raw)
			case pktNACK:
				c.handleNACKPacket(raw)
			}
		}
	}
}

// handleDataPacket processes a DATA or RETX packet.
// Format: type(1) + pktSeq(2) + chunkPayload
func (c *RTPConn) handleDataPacket(raw []byte) {
	if len(raw) < 3 {
		return
	}
	pktSeq := binary.BigEndian.Uint16(raw[1:3])
	chunkPayload := raw[3:]

	// Record in FEC decoder pool and NACK tracker.
	c.fecDecoder.AddData(pktSeq, chunkPayload)
	c.nackTrack.Record(pktSeq)

	// Periodically prune old FEC data.
	nrs := c.getNextReadSeq()
	if pktSeq%64 == 0 {
		c.fecDecoder.Prune(nrs)
		c.nackTrack.Prune(nrs)
	}

	// Forward to reassembly.
	select {
	case c.readCh <- chunkPayload:
	case <-c.closeCh:
	}
}

// handleFECPacket processes a FEC parity packet.
// Format: type(1) + firstSeq(2) + stride(1) + count(1) + xorPayload
func (c *RTPConn) handleFECPacket(raw []byte) {
	if len(raw) < 5 {
		return
	}
	firstSeq := binary.BigEndian.Uint16(raw[1:3])
	stride := raw[3]
	count := int(raw[4])
	parity := raw[5:]

	recoveredSeq, recoveredPayload, ok := c.fecDecoder.Recover(firstSeq, stride, count, parity)
	if ok {
		slog.Info("rtpconn: FEC recovered packet", "pktSeq", recoveredSeq)
		// Record recovery in NACK tracker so it doesn't send unnecessary NACKs.
		c.nackTrack.Resolve(recoveredSeq)
		// Also add recovered data to FEC pool for potential future FEC groups.
		c.fecDecoder.AddData(recoveredSeq, recoveredPayload)

		// Forward recovered chunk to reassembly.
		select {
		case c.readCh <- recoveredPayload:
		case <-c.closeCh:
		}
	}
}

// handleNACKPacket processes a NACK from the remote peer.
// Format: type(1) + count(1) + [pktSeq(2)]*count
func (c *RTPConn) handleNACKPacket(raw []byte) {
	if len(raw) < 2 {
		return
	}
	count := int(raw[1])
	if len(raw) < 2+count*2 {
		return
	}
	c.nackRecv.Add(int64(count)) // count each requested seq for adaptive pacing

	for i := 0; i < count; i++ {
		off := 2 + i*2
		pktSeq := binary.BigEndian.Uint16(raw[off : off+2])

		chunkPayload := c.retxBuf.Get(pktSeq)
		if chunkPayload == nil {
			continue
		}

		// Build RETX packet: type(1) + pktSeq(2) + chunkPayload
		retxPkt := make([]byte, 3+len(chunkPayload))
		retxPkt[0] = pktRETX
		binary.BigEndian.PutUint16(retxPkt[1:3], pktSeq)
		copy(retxPkt[3:], chunkPayload)

		if err := c.sendRawVP8(retxPkt); err != nil {
			slog.Info("rtpconn: retransmit failed", "pktSeq", pktSeq, "err", err)
			return
		}
		slog.Info("rtpconn: retransmitted", "pktSeq", pktSeq)
	}
}

// nackLoop periodically checks for gaps and sends NACK packets to the remote peer.
func (c *RTPConn) nackLoop() {
	ticker := time.NewTicker(nackCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeCh:
			return
		case <-ticker.C:
			nacks := c.nackTrack.GetNACKs()
			if len(nacks) == 0 {
				continue
			}

			// Build NACK packet: type(1) + count(1) + [pktSeq(2)]*count
			pkt := make([]byte, 2+len(nacks)*2)
			pkt[0] = pktNACK
			pkt[1] = byte(len(nacks))
			for i, seq := range nacks {
				binary.BigEndian.PutUint16(pkt[2+i*2:], seq)
			}

			if err := c.sendRawVP8(pkt); err != nil {
				slog.Info("rtpconn: send NACK failed", "err", err)
			} else {
				slog.Info("rtpconn: sent NACK", "count", len(nacks), "seqs", nacks)
			}
		}
	}
}

// paceAdjustLoop implements AIMD (Additive Increase / Multiplicative Decrease)
// for the VP8 write pacing interval. It uses the rate of incoming NACK packets
// (peer requesting retransmissions of our frames) as a congestion signal:
//   - Low NACK rate  → decrease pace (send faster)
//   - High NACK rate → increase pace (back off)
func (c *RTPConn) paceAdjustLoop() {
	ticker := time.NewTicker(paceAdjustInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.closeCh:
			return
		case <-ticker.C:
			nacks := c.nackRecv.Swap(0)
			writes := c.writeCount.Swap(0)
			cur := time.Duration(c.currentPace.Load())

			// No writes in this window — reset to default pace.
			// This prevents the pace from racing to minPace during idle,
			// which would overwhelm the SFU when data starts flowing.
			if writes == 0 {
				if cur != defaultPace {
					c.currentPace.Store(int64(defaultPace))
					slog.Debug("rtpconn: pace reset to default (idle)",
						"prev", cur)
				}
				continue
			}

			var next time.Duration
			if nacks > nackRateThreshold {
				// Multiplicative Decrease: back off quickly on congestion.
				next = time.Duration(float64(cur) * paceMultiplier)
				if next > maxPace {
					next = maxPace
				}
			} else {
				// Additive Increase: cautiously speed up when channel is clear.
				next = cur - paceDecrease
				if next < minPace {
					next = minPace
				}
			}

			if next != cur {
				c.currentPace.Store(int64(next))
				slog.Info("rtpconn: adaptive pace",
					"prev", cur, "next", next, "nacks", nacks, "writes", writes)
			}
		}
	}
}

// sendRawVP8 wraps a reliability-layer packet in VP8 and writes to the publisher track.
// All VP8 writes are paced to avoid overwhelming the SFU with burst frames.
func (c *RTPConn) sendRawVP8(payload []byte) error {
	pace := time.Duration(c.currentPace.Load())
	c.seqMu.Lock()
	elapsed := time.Since(c.lastWrite)
	c.seqMu.Unlock()
	if elapsed < pace {
		time.Sleep(pace - elapsed)
	}

	frame := buildVP8Frame(payload, c.obfKey)
	err := c.pubTrack.WriteSample(media.Sample{
		Data:     frame,
		Duration: 33 * time.Millisecond,
	})

	c.seqMu.Lock()
	c.lastWrite = time.Now()
	c.seqMu.Unlock()

	c.writeCount.Add(1)
	return err
}

// seqAfter returns true if a is after b in uint16 sequence space.
func seqAfter(a, b uint16) bool {
	return int16(a-b) > 0
}

// seqClose returns true if a and b are within 50 of each other in uint16 sequence space.
func seqClose(a, b uint16) bool {
	diff := int16(a - b)
	return diff >= -50 && diff <= 50
}

// getNextReadSeq returns the current nextReadSeq (thread-safe).
func (c *RTPConn) getNextReadSeq() uint16 {
	c.nrsMu.Lock()
	v := c.nextReadSeq
	c.nrsMu.Unlock()
	return v
}

// incNextReadSeq increments nextReadSeq (thread-safe).
func (c *RTPConn) incNextReadSeq() {
	c.nrsMu.Lock()
	c.nextReadSeq++
	c.nrsMu.Unlock()
}

// HandleTrack should be called from OnTrack to process incoming video tracks.
// It reassembles VP8 frames from multiple RTP packets before extracting VPN data.
// When multiple tracks exist (stale participants), locks to the first track
// that delivers valid VPN data with a matching sequence number.
// When a track closes (e.g., due to re-negotiation), the SSRC lock is released
// so the replacement track can take over.
func (c *RTPConn) HandleTrack(track *webrtc.TrackRemote) {
	ssrc := uint32(track.SSRC())

	go func() {
		defer func() {
			c.lockedMu.Lock()
			if c.lockedSSRC == ssrc {
				slog.Info("rtpconn: track closed, unlocking SSRC", "ssrc", ssrc)
				c.lockedSSRC = 0
			}
			c.lockedMu.Unlock()
		}()

		var frameBuf []byte

		for {
			select {
			case <-c.closeCh:
				return
			default:
			}

			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}

			c.lockedMu.Lock()
			locked := c.lockedSSRC
			c.lockedMu.Unlock()
			if locked != 0 && locked != ssrc {
				continue
			}

			vp8Payload := stripVP8Descriptor(pkt.Payload)
			if vp8Payload == nil {
				continue
			}

			isStart := len(pkt.Payload) > 0 && (pkt.Payload[0]&0x10) != 0

			if isStart {
				frameBuf = append(frameBuf[:0], vp8Payload...)
			} else {
				frameBuf = append(frameBuf, vp8Payload...)
			}

			if !pkt.Marker {
				continue
			}

			data := extractVP8Data(frameBuf, c.obfKey)
			if data == nil || len(data) == 0 {
				continue
			}

			// SSRC locking: verify the packet looks like valid VPN data.
			// For DATA/RETX packets, extract writeSeq from chunkPayload and compare
			// with nextReadSeq (both in write-sequence space).
			// Format: pktType(1) + pktSeq(2) + writeSeq(2) + idx(1) + total(1) + data
			if locked == 0 && len(data) >= 7 {
				pktType := data[0]
				if pktType == pktDATA || pktType == pktRETX {
					writeSeq := binary.BigEndian.Uint16(data[3:5])
					nrs := c.getNextReadSeq()
					c.lockedMu.Lock()
					if c.lockedSSRC == 0 {
						if nrs == 0 || seqClose(writeSeq, nrs) {
							c.lockedSSRC = ssrc
							// Sync nextReadSeq to the peer's current writeSeq so
							// reassembly doesn't stall waiting for seq 0 after retries.
							if nrs == 0 && writeSeq != 0 {
								c.nrsMu.Lock()
								c.nextReadSeq = writeSeq
								c.nrsMu.Unlock()
							}
							slog.Info("rtpconn: locked to SSRC", "ssrc", ssrc, "writeSeq", writeSeq, "nextReadSeq", c.getNextReadSeq())
						} else {
							c.lockedMu.Unlock()
							slog.Info("rtpconn: rejected SSRC (seq mismatch)", "ssrc", ssrc, "writeSeq", writeSeq, "nextReadSeq", nrs)
							continue
						}
					}
					c.lockedMu.Unlock()
				} else if pktType == pktFEC || pktType == pktNACK {
					if locked == 0 {
						continue
					}
				}
			}

			// Re-check: another goroutine may have locked to a different SSRC
			// between the initial check (locked == 0) and now. Drop data if
			// we're not the locked SSRC to prevent mixing data from multiple tracks.
			c.lockedMu.Lock()
			locked = c.lockedSSRC
			c.lockedMu.Unlock()
			if locked != 0 && locked != ssrc {
				continue
			}

			// Copy data before sending — extractVP8Data returns a sub-slice of
			// frameBuf which gets overwritten on the next RTP packet.
			dataCopy := make([]byte, len(data))
			copy(dataCopy, data)

			// Signal that we received valid VP8 data (correct obfKey).
			select {
			case c.validDataCh <- struct{}{}:
			default:
			}

			select {
			case c.rawPktCh <- dataCopy:
			case <-c.closeCh:
				return
			}
		}
	}()
}

// stripVP8Descriptor removes the VP8 RTP payload descriptor and returns
// the raw VP8 bitstream data. Returns nil if the payload is too short.
func stripVP8Descriptor(payload []byte) []byte {
	if len(payload) < 1 {
		return nil
	}

	offset := 1
	x := payload[0] & 0x80 // extension bit
	if x != 0 {
		if offset >= len(payload) {
			return nil
		}
		ext := payload[offset]
		offset++
		if ext&0x80 != 0 { // PictureID
			if offset >= len(payload) {
				return nil
			}
			if payload[offset]&0x80 != 0 {
				offset += 2 // 16-bit PictureID
			} else {
				offset++ // 8-bit PictureID
			}
		}
		if ext&0x40 != 0 { // TL0PICIDX
			offset++
		}
		if ext&0x20 != 0 { // TID/Y/KEYIDX
			offset++
		}
	}

	if offset >= len(payload) {
		return nil
	}
	return payload[offset:]
}

func (c *RTPConn) Read(p []byte) (int, error) {
	select {
	case <-c.closeCh:
		return 0, io.EOF
	case data, ok := <-c.orderedCh:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, data)
		return n, nil
	}
}

func (c *RTPConn) Write(p []byte) (int, error) {
	select {
	case <-c.closeCh:
		return 0, io.ErrClosedPipe
	default:
	}

	seq := c.nextSeq()
	totalChunks := (len(p) + maxVP8Data - 1) / maxVP8Data
	for i := 0; i < totalChunks; i++ {
		off := i * maxVP8Data
		end := off + maxVP8Data
		if end > len(p) {
			end = len(p)
		}
		chunk := p[off:end]

		// Build chunkPayload: writeSeq(2) + idx(1) + total(1) + data
		chunkPayload := make([]byte, 4+len(chunk))
		binary.BigEndian.PutUint16(chunkPayload[0:2], seq)
		chunkPayload[2] = byte(i)
		chunkPayload[3] = byte(totalChunks)
		copy(chunkPayload[4:], chunk)

		// Assign a packet-level sequence number.
		pktSeq := c.nextPktSeqNum()

		// Store in retransmit buffer for NACK handling.
		c.retxBuf.Store(pktSeq, chunkPayload)

		// Build reliability-layer packet: type(1) + pktSeq(2) + chunkPayload
		pkt := make([]byte, 3+len(chunkPayload))
		pkt[0] = pktDATA
		binary.BigEndian.PutUint16(pkt[1:3], pktSeq)
		copy(pkt[3:], chunkPayload)

		// Send via paced VP8 writer.
		if err := c.sendRawVP8(pkt); err != nil {
			return off, fmt.Errorf("write VP8 sample: %w", err)
		}

		// Feed to FEC encoder. If a group is complete, send the parity packet.
		xorPayload, firstSeq, count, fecReady := c.fecEncoder.AddPacket(pktSeq, chunkPayload)
		if fecReady {
			fecPkt := make([]byte, 5+len(xorPayload))
			fecPkt[0] = pktFEC
			binary.BigEndian.PutUint16(fecPkt[1:3], firstSeq)
			fecPkt[3] = fecStride
			fecPkt[4] = byte(count)
			copy(fecPkt[5:], xorPayload)

			if fecErr := c.sendRawVP8(fecPkt); fecErr != nil {
				slog.Info("rtpconn: send FEC failed", "err", fecErr)
			}
		}
	}
	return len(p), nil
}

// ResetSSRC performs a full state reset for switching to a new peer after pin.
// Clears SSRC lock, resets all sequence counters, and signals background loops
// to discard stale data from the previous peer.
func (c *RTPConn) ResetSSRC() {
	// Clear SSRC lock — allow locking to new peer's track.
	c.lockedMu.Lock()
	c.lockedSSRC = 0
	c.lockedMu.Unlock()

	// Reset read sequence — new peer starts from writeSeq=0.
	c.nrsMu.Lock()
	c.nextReadSeq = 0
	c.nrsMu.Unlock()

	// Reset write sequences — our writes should start from 0 for the new peer.
	c.seqMu.Lock()
	c.seqNum = 0
	c.lastWrite = time.Time{}
	c.seqMu.Unlock()

	c.pktSeqMu.Lock()
	c.nextPktSeq = 0
	c.pktSeqMu.Unlock()

	// Reset reliability components (thread-safe via their own mutexes).
	c.nackTrack.Reset()
	c.retxBuf.Reset()
	c.fecEncoder = NewFECEncoder() // safe: Write() not called during pin

	// Signal reliabilityLoop to clear FEC decoder state.
	select {
	case c.resetReliabilityCh <- struct{}{}:
	default:
	}
	// Signal reassemblyLoop to clear reassembly buffers.
	select {
	case c.resetReassemblyCh <- struct{}{}:
	default:
	}
	// Drain validDataCh so WaitValidData re-waits for fresh data.
	select {
	case <-c.validDataCh:
	default:
	}
}

// WaitValidData blocks until the first valid VP8 data is received after a
// ResetSSRC, or the context expires. Used to probe whether a pinned candidate
// is the real peer (correct obfuscation key) or a ghost.
func (c *RTPConn) WaitValidData(ctx context.Context) error {
	select {
	case <-c.validDataCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		return fmt.Errorf("connection closed")
	}
}

func (c *RTPConn) Close() error {
	c.once.Do(func() {
		close(c.closeCh)
	})
	return nil
}

// VP8 magic marker to identify our VPN data frames.
var vpnMagic = []byte{0xCA, 0x11, 0xDA, 0x7A} // "call-data"

// vp8Width and vp8Height are the dimensions encoded in VP8 keyframes.
const (
	vp8Width  = 320
	vp8Height = 240
)

// obfuscateVP8Payload XORs the data area of a VP8 frame (from offset 10)
// with a repeating 32-byte key. This hides vpnMagic, packet type headers,
// and structured data from the SFU and any DPI on the media path.
func obfuscateVP8Payload(buf []byte, key [32]byte) {
	const vp8HeaderLen = 10 // 3 (tag) + 3 (start code) + 4 (dimensions)
	for i := vp8HeaderLen; i < len(buf); i++ {
		buf[i] ^= key[(i-vp8HeaderLen)%32]
	}
}

// buildVP8Frame wraps VPN data in a VP8 keyframe and XOR-obfuscates the payload.
func buildVP8Frame(data []byte, key [32]byte) []byte {
	partLen := uint32(3 + 4 + 4 + 2 + len(data))
	tag0 := byte(0x10) | byte((partLen&0x7)<<5)
	tag1 := byte((partLen >> 3) & 0xFF)
	tag2 := byte((partLen >> 11) & 0xFF)

	wLo := byte(vp8Width & 0xFF)
	wHi := byte((vp8Width >> 8) & 0x3F)
	hLo := byte(vp8Height & 0xFF)
	hHi := byte((vp8Height >> 8) & 0x3F)

	totalPayload := 4 + 2 + len(data)

	const minFrameSize = 1024
	padLen := 0
	if totalPayload < minFrameSize-10 {
		padLen = minFrameSize - 10 - totalPayload
	}

	buf := make([]byte, 0, 3+3+4+totalPayload+padLen)
	buf = append(buf, tag0, tag1, tag2)
	buf = append(buf, 0x9d, 0x01, 0x2a)
	buf = append(buf, wLo, wHi)
	buf = append(buf, hLo, hHi)
	buf = append(buf, vpnMagic...)
	lenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBuf, uint16(len(data)))
	buf = append(buf, lenBuf...)
	buf = append(buf, data...)
	if padLen > 0 {
		buf = append(buf, make([]byte, padLen)...)
	}

	// XOR-obfuscate everything after the VP8 header.
	obfuscateVP8Payload(buf, key)

	return buf
}

// extractVP8Data extracts VPN data from a reassembled VP8 bitstream.
// The payload area is XOR-deobfuscated before checking the magic marker.
func extractVP8Data(vp8 []byte, key [32]byte) []byte {
	if len(vp8) < 3+3+4+4+2 {
		return nil
	}

	if vp8[0]&0x01 != 0 {
		return nil
	}

	if vp8[3] != 0x9d || vp8[4] != 0x01 || vp8[5] != 0x2a {
		return nil
	}

	// Deobfuscate the payload area (from offset 10) in-place.
	obfuscateVP8Payload(vp8, key)

	magicOffset := 3 + 3 + 4
	if vp8[magicOffset] != vpnMagic[0] ||
		vp8[magicOffset+1] != vpnMagic[1] ||
		vp8[magicOffset+2] != vpnMagic[2] ||
		vp8[magicOffset+3] != vpnMagic[3] {
		return nil
	}

	lenOffset := magicOffset + 4
	dataLen := int(binary.BigEndian.Uint16(vp8[lenOffset:]))
	dataOffset := lenOffset + 2

	if dataOffset+dataLen > len(vp8) {
		return nil
	}

	return vp8[dataOffset : dataOffset+dataLen]
}
