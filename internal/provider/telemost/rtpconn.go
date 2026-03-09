package telemost

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// RTPConn wraps publisher video track (write) and subscriber video track (read)
// into a bidirectional io.ReadWriteCloser. VPN data is encoded as VP8 keyframes
// and forwarded by the SFU through its standard RTP media pipeline.
//
// When multiple video tracks are received (e.g. stale participants in the room),
// RTPConn locks to the first track that delivers valid VPN data and ignores others.
// maxVP8Data is the maximum VPN data per VP8 frame to ensure each frame fits
// in a single RTP packet. RTP MTU ~1200 bytes minus VP8 RTP descriptor (~5 bytes)
// minus VP8 header (16 bytes) minus chunk header (4 bytes).
const maxVP8Data = 1100

// writePace is the minimum interval between consecutive VP8 frame writes.
// Without pacing, burst writes overwhelm the SFU's video forwarding pipeline.
const writePace = 10 * time.Millisecond

type RTPConn struct {
	pubTrack *webrtc.TrackLocalStaticSample // publisher video track (write path)
	readCh   chan []byte                    // decoded payloads from subscriber tracks
	once     sync.Once
	closeCh  chan struct{}

	lockedMu   sync.Mutex
	lockedSSRC uint32 // SSRC of the track we've locked to (0 = not yet locked)

	seqMu    sync.Mutex
	seqNum   uint16 // write sequence counter
	lastWrite time.Time // last VP8 frame write time for pacing

	// Reassembly state (single-goroutine access from reassemblyLoop).
	reassembly  map[uint16]*reassemblyBuf
	nextReadSeq uint16     // next expected sequence number for ordered delivery
	nrsMu       sync.Mutex // protects nextReadSeq for cross-goroutine reads
	orderedCh   chan []byte
}

type reassemblyBuf struct {
	chunks [][]byte
	total  int
	got    int
}

// NewRTPConn creates a bidirectional connection using RTP video tracks.
func NewRTPConn(pubTrack *webrtc.TrackLocalStaticSample) *RTPConn {
	c := &RTPConn{
		pubTrack:   pubTrack,
		readCh:     make(chan []byte, 256),
		closeCh:    make(chan struct{}),
		reassembly: make(map[uint16]*reassemblyBuf),
		orderedCh:  make(chan []byte, 64),
	}
	go c.reassemblyLoop()
	return c
}

func (c *RTPConn) nextSeq() uint16 {
	c.seqMu.Lock()
	s := c.seqNum
	c.seqNum++
	c.seqMu.Unlock()
	return s
}

// reassemblyTimeout is how long we wait for a missing sequence before skipping it.
// If the SFU drops a VP8 frame, we can't stall forever — skip after this timeout.
const reassemblyTimeout = 100 * time.Millisecond

// reassemblyLoop reads raw chunks from readCh, reassembles multi-chunk
// messages, and delivers them in sequence order to orderedCh.
// If a sequence is missing for longer than reassemblyTimeout, it is skipped
// to prevent a single lost frame from stalling the entire data pipeline.
func (c *RTPConn) reassemblyLoop() {
	var stallTimer *time.Timer
	var stallCh <-chan time.Time // nil when no stall detected

	for {
		select {
		case <-c.closeCh:
			if stallTimer != nil {
				stallTimer.Stop()
			}
			return

		case <-stallCh:
			// Timeout waiting for nextReadSeq — skip missing/incomplete sequences.
			skipped := 0
			for {
				rb, exists := c.reassembly[c.nextReadSeq]
				if exists && rb.got >= rb.total {
					break // this seq is complete, deliverReady will handle it
				}
				// Check if there's any higher seq buffered.
				hasHigher := false
				for seq := range c.reassembly {
					if seqAfter(seq, c.nextReadSeq) {
						hasHigher = true
						break
					}
				}
				if !hasHigher {
					break // nothing buffered ahead, just wait
				}
				// Skip this missing or incomplete sequence.
				delete(c.reassembly, c.nextReadSeq)
				skipped++
				c.incNextReadSeq()
			}
			if skipped > 0 {
				slog.Info("rtpconn: skipped lost/incomplete sequences", "count", skipped, "nextReadSeq", c.nextReadSeq)
			}
			stallCh = nil

			// Try to deliver what we can now.
			c.deliverReady()

			// Check if still stalled on next missing seq.
			if c.hasBufferedAhead() {
				stallTimer.Reset(reassemblyTimeout)
				stallCh = stallTimer.C
			}

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

			// Try to deliver completed sequences in order.
			c.deliverReady()

			// If we have buffered data ahead of nextReadSeq but nextReadSeq is missing,
			// start a stall timer to skip it after timeout.
			if stallCh == nil && c.hasBufferedAhead() {
				if stallTimer == nil {
					stallTimer = time.NewTimer(reassemblyTimeout)
				} else {
					stallTimer.Reset(reassemblyTimeout)
				}
				stallCh = stallTimer.C
			}
			// If we caught up (no stall), cancel the timer.
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
			return false // current is ready, deliverReady will handle it
		}
	}
	for seq := range c.reassembly {
		if seqAfter(seq, c.nextReadSeq) {
			return true
		}
	}
	return false
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
			// When this track closes, unlock SSRC if we held the lock.
			c.lockedMu.Lock()
			if c.lockedSSRC == ssrc {
				slog.Info("rtpconn: track closed, unlocking SSRC", "ssrc", ssrc)
				c.lockedSSRC = 0
			}
			c.lockedMu.Unlock()
		}()

		var frameBuf []byte // accumulates VP8 payload across RTP packets

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

			// If we've already locked to a different track, ignore this one.
			c.lockedMu.Lock()
			locked := c.lockedSSRC
			c.lockedMu.Unlock()
			if locked != 0 && locked != ssrc {
				continue
			}

			// Strip VP8 RTP payload descriptor to get raw VP8 bitstream.
			vp8Payload := stripVP8Descriptor(pkt.Payload)
			if vp8Payload == nil {
				continue
			}

			// Check S bit (start of VP8 partition) in the first byte.
			isStart := len(pkt.Payload) > 0 && (pkt.Payload[0]&0x10) != 0

			if isStart {
				// New frame — reset buffer.
				frameBuf = append(frameBuf[:0], vp8Payload...)
			} else {
				// Continuation — append to buffer.
				frameBuf = append(frameBuf, vp8Payload...)
			}

			// Marker bit = last packet of this frame.
			if !pkt.Marker {
				continue
			}

			// Frame complete — extract VPN data.
			data := extractVP8Data(frameBuf)
			if data == nil || len(data) == 0 {
				continue // skip non-VPN frames and keepalive frames
			}

			// Lock to this track — verify seq is reasonable to avoid ghost data.
			if locked == 0 && len(data) >= 2 {
				chunkSeq := binary.BigEndian.Uint16(data[0:2])
				nrs := c.getNextReadSeq()
				c.lockedMu.Lock()
				if c.lockedSSRC == 0 {
					if nrs == 0 || seqClose(chunkSeq, nrs) {
						c.lockedSSRC = ssrc
						slog.Info("rtpconn: locked to SSRC", "ssrc", ssrc, "chunkSeq", chunkSeq, "nextReadSeq", nrs)
					} else {
						c.lockedMu.Unlock()
						slog.Info("rtpconn: rejected SSRC (seq mismatch)", "ssrc", ssrc, "chunkSeq", chunkSeq, "nextReadSeq", nrs)
						continue
					}
				}
				c.lockedMu.Unlock()
			}

			select {
			case c.readCh <- data:
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

	// Each Write is chunked into VP8 frames that fit in a single RTP packet
	// (~1200 bytes). This avoids multi-packet VP8 frames which are fragile
	// against RTP reordering in the SFU.
	// Chunks include a 4-byte sequence header so the receiver can reassemble
	// them in order even if VP8 frames arrive out of order.
	seq := c.nextSeq()
	totalChunks := (len(p) + maxVP8Data - 1) / maxVP8Data
	for i := 0; i < totalChunks; i++ {
		// Pace writes to avoid overwhelming the SFU.
		c.seqMu.Lock()
		elapsed := time.Since(c.lastWrite)
		c.seqMu.Unlock()
		if elapsed < writePace {
			time.Sleep(writePace - elapsed)
		}

		off := i * maxVP8Data
		end := off + maxVP8Data
		if end > len(p) {
			end = len(p)
		}
		chunk := p[off:end]

		// Build chunked payload: seq(2) + idx(1) + total(1) + data
		chunked := make([]byte, 4+len(chunk))
		binary.BigEndian.PutUint16(chunked[0:2], seq)
		chunked[2] = byte(i)
		chunked[3] = byte(totalChunks)
		copy(chunked[4:], chunk)

		frame := buildVP8Frame(chunked)
		err := c.pubTrack.WriteSample(media.Sample{
			Data:     frame,
			Duration: 33 * time.Millisecond,
		})

		c.seqMu.Lock()
		c.lastWrite = time.Now()
		c.seqMu.Unlock()

		if err != nil {
			return off, fmt.Errorf("write VP8 sample: %w", err)
		}
	}
	return len(p), nil
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
// Using realistic dimensions so the SFU treats our track as active video.
const (
	vp8Width  = 320
	vp8Height = 240
)

// buildVP8Frame wraps VPN data in a VP8 keyframe.
// Format: VP8 frame tag (3 bytes) + start code (3 bytes) + dimensions (4 bytes) +
// magic (4 bytes) + length (2 bytes) + data + padding
func buildVP8Frame(data []byte) []byte {
	// VP8 keyframe frame tag:
	// Bit 0: frame_type = 0 (keyframe)
	// Bits 1-3: version = 0
	// Bit 4: show_frame = 1
	// Bits 5-23: first_partition_length (set to remaining bytes after the tag)
	partLen := uint32(3 + 4 + 4 + 2 + len(data)) // start_code + dims + magic + len + data
	tag0 := byte(0x10) | byte((partLen&0x7)<<5)
	tag1 := byte((partLen >> 3) & 0xFF)
	tag2 := byte((partLen >> 11) & 0xFF)

	// VP8 dimensions encoding:
	// width is 14 bits in bytes [6:7] (little-endian), top 2 bits = horizontal scale
	// height is 14 bits in bytes [8:9] (little-endian), top 2 bits = vertical scale
	wLo := byte(vp8Width & 0xFF)
	wHi := byte((vp8Width >> 8) & 0x3F) // 14-bit, no scale
	hLo := byte(vp8Height & 0xFF)
	hHi := byte((vp8Height >> 8) & 0x3F) // 14-bit, no scale

	totalPayload := 4 + 2 + len(data) // magic + len + data

	// All frames are padded to at least 1KB so the SFU treats them as
	// real video and doesn't drop them. With maxVP8Data=1100, the largest
	// frame is 16+1100=1116 bytes (fits in one RTP packet ~1200 bytes).
	const minFrameSize = 1024
	padLen := 0
	if totalPayload < minFrameSize-10 { // 10 = tag+startcode+dims
		padLen = minFrameSize - 10 - totalPayload
	}

	buf := make([]byte, 0, 3+3+4+totalPayload+padLen)
	buf = append(buf, tag0, tag1, tag2)   // frame tag
	buf = append(buf, 0x9d, 0x01, 0x2a)   // VP8 start code
	buf = append(buf, wLo, wHi)           // width = 320
	buf = append(buf, hLo, hHi)           // height = 240
	buf = append(buf, vpnMagic...)         // magic marker
	lenBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(lenBuf, uint16(len(data)))
	buf = append(buf, lenBuf...)           // data length
	buf = append(buf, data...)             // actual VPN data
	if padLen > 0 {
		buf = append(buf, make([]byte, padLen)...) // zero padding
	}
	return buf
}

// extractVP8Data extracts VPN data from a reassembled VP8 bitstream.
// The input is raw VP8 data (RTP payload descriptor already stripped).
// Returns nil if the data is not a VPN data frame.
func extractVP8Data(vp8 []byte) []byte {
	// VP8 keyframe: frame tag (3 bytes) + start code (3 bytes) + dims (4 bytes) + magic (4 bytes) + len (2 bytes) + data
	if len(vp8) < 3+3+4+4+2 {
		return nil
	}

	// Check frame_type = keyframe (bit 0 of byte 0)
	if vp8[0]&0x01 != 0 {
		return nil // not a keyframe
	}

	// Check VP8 start code
	if vp8[3] != 0x9d || vp8[4] != 0x01 || vp8[5] != 0x2a {
		return nil
	}

	// Check magic
	magicOffset := 3 + 3 + 4 // frame_tag + start_code + dimensions
	if vp8[magicOffset] != vpnMagic[0] ||
		vp8[magicOffset+1] != vpnMagic[1] ||
		vp8[magicOffset+2] != vpnMagic[2] ||
		vp8[magicOffset+3] != vpnMagic[3] {
		return nil
	}

	// Read length
	lenOffset := magicOffset + 4
	dataLen := int(binary.BigEndian.Uint16(vp8[lenOffset:]))
	dataOffset := lenOffset + 2

	if dataOffset+dataLen > len(vp8) {
		return nil
	}

	return vp8[dataOffset : dataOffset+dataLen]
}
