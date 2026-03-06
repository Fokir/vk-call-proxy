package mux

import (
	"encoding/binary"
	"errors"
	"io"
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
