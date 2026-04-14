package telemost

import (
	"sync"
	"time"
)

const (
	// retxBufSize: при writePace ≈5ms и maxVP8Data=1100 это даёт ~2048×5ms = ~10s
	// окна retransmit — согласовано с reassemblyTimeout=10s. На N=4 соединениях и
	// payload ~1100б это ~9 MB RAM на клиенте, приемлемо.
	retxBufSize  = 2048
	nackDelay    = 30 * time.Millisecond  // initial wait before first NACK (give FEC time)
	nackRetry    = 100 * time.Millisecond // re-NACK interval if packet still missing
	maxNACKSeqs  = 32                     // max pktSeqs per NACK message
)

// RetransmitBuffer stores recent sent chunkPayloads for retransmission on NACK.
type RetransmitBuffer struct {
	mu  sync.Mutex
	buf [retxBufSize]retxEntry
}

type retxEntry struct {
	pktSeq  uint16
	payload []byte
	valid   bool
}

// Store saves a data packet for potential retransmission.
func (rb *RetransmitBuffer) Store(pktSeq uint16, chunkPayload []byte) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	idx := pktSeq % retxBufSize
	rb.buf[idx] = retxEntry{
		pktSeq:  pktSeq,
		payload: append([]byte(nil), chunkPayload...),
		valid:   true,
	}
}

// Reset clears all stored retransmission data for a new peer connection.
func (rb *RetransmitBuffer) Reset() {
	rb.mu.Lock()
	rb.buf = [retxBufSize]retxEntry{}
	rb.mu.Unlock()
}

// Get retrieves a stored packet. Returns nil if not found or overwritten.
func (rb *RetransmitBuffer) Get(pktSeq uint16) []byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	idx := pktSeq % retxBufSize
	e := rb.buf[idx]
	if e.valid && e.pktSeq == pktSeq {
		return append([]byte(nil), e.payload...)
	}
	return nil
}

// NACKTracker detects gaps in received pktSeq and reports them with retries.
// Unlike one-shot NACKs, it keeps reporting missing pktSeqs every nackRetry
// interval until they are resolved (received or FEC-recovered).
type NACKTracker struct {
	mu          sync.Mutex
	gaps        map[uint16]*gapEntry // pktSeq → gap info
	received    map[uint16]bool      // pktSeqs received but ahead of nextExpect
	nextExpect  uint16
	initialized bool
}

type gapEntry struct {
	detected time.Time // when the gap was first detected
	lastNack time.Time // when we last sent a NACK for this
	nacked   bool      // whether we've sent at least one NACK
}

// NewNACKTracker creates a new NACK tracker.
func NewNACKTracker() *NACKTracker {
	return &NACKTracker{
		gaps:     make(map[uint16]*gapEntry),
		received: make(map[uint16]bool),
	}
}

// Record registers a received pktSeq and detects gaps.
func (nt *NACKTracker) Record(pktSeq uint16) {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	if !nt.initialized {
		nt.nextExpect = pktSeq + 1
		nt.initialized = true
		return
	}

	delete(nt.gaps, pktSeq)
	nt.received[pktSeq] = true

	if seqAfter(pktSeq, nt.nextExpect) {
		now := time.Now()
		for s := nt.nextExpect; s != pktSeq; s++ {
			if !nt.received[s] {
				if _, exists := nt.gaps[s]; !exists {
					nt.gaps[s] = &gapEntry{detected: now}
				}
			}
		}
	}

	for nt.received[nt.nextExpect] {
		delete(nt.received, nt.nextExpect)
		nt.nextExpect++
	}
}

// Resolve removes a pktSeq from gaps (e.g., after FEC recovery).
func (nt *NACKTracker) Resolve(pktSeq uint16) {
	nt.mu.Lock()
	delete(nt.gaps, pktSeq)
	nt.received[pktSeq] = true

	for nt.received[nt.nextExpect] {
		delete(nt.received, nt.nextExpect)
		nt.nextExpect++
	}
	nt.mu.Unlock()
}

// GetNACKs returns pktSeqs ready for (re-)NACKing:
// - First NACK after nackDelay from detection
// - Subsequent re-NACKs every nackRetry interval
// Does NOT remove from tracking — keeps retrying until Resolve/Record.
func (nt *NACKTracker) GetNACKs() []uint16 {
	nt.mu.Lock()
	defer nt.mu.Unlock()

	now := time.Now()
	var nacks []uint16
	for seq, ge := range nt.gaps {
		if !ge.nacked {
			// First NACK: wait nackDelay from detection.
			if now.Sub(ge.detected) >= nackDelay {
				ge.nacked = true
				ge.lastNack = now
				nacks = append(nacks, seq)
			}
		} else {
			// Re-NACK: wait nackRetry from last NACK.
			if now.Sub(ge.lastNack) >= nackRetry {
				ge.lastNack = now
				nacks = append(nacks, seq)
			}
		}
		if len(nacks) >= maxNACKSeqs {
			break
		}
	}
	return nacks
}

// Reset clears all tracker state for a new peer connection.
func (nt *NACKTracker) Reset() {
	nt.mu.Lock()
	nt.gaps = make(map[uint16]*gapEntry)
	nt.received = make(map[uint16]bool)
	nt.nextExpect = 0
	nt.initialized = false
	nt.mu.Unlock()
}

// Prune removes gap entries older than the given minimum pktSeq.
func (nt *NACKTracker) Prune(minSeq uint16) {
	nt.mu.Lock()
	defer nt.mu.Unlock()
	for seq := range nt.gaps {
		if seqAfter(minSeq, seq) {
			delete(nt.gaps, seq)
		}
	}
	for seq := range nt.received {
		if seqAfter(minSeq, seq) {
			delete(nt.received, seq)
		}
	}
}
