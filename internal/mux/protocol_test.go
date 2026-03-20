package mux

import (
	"bytes"
	"testing"
)

func TestMarshalUnmarshalFrame(t *testing.T) {
	f := Frame{
		StreamID: 42,
		Type:     FrameData,
		Sequence: 100,
		Payload:  []byte("hello world"),
	}

	data, err := f.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}

	if len(data) != 13+len(f.Payload) {
		t.Fatalf("expected %d bytes, got %d", 13+len(f.Payload), len(data))
	}

	decoded, err := ReadFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}

	if decoded.StreamID != f.StreamID {
		t.Fatalf("StreamID: got %d, want %d", decoded.StreamID, f.StreamID)
	}
	if decoded.Type != f.Type {
		t.Fatalf("Type: got %d, want %d", decoded.Type, f.Type)
	}
	if decoded.Sequence != f.Sequence {
		t.Fatalf("Sequence: got %d, want %d", decoded.Sequence, f.Sequence)
	}
	if !bytes.Equal(decoded.Payload, f.Payload) {
		t.Fatalf("Payload mismatch")
	}
}

func TestFrameAllTypes(t *testing.T) {
	for _, ft := range []uint8{FrameData, FrameOpen, FrameClose, FramePing, FramePong} {
		f := Frame{StreamID: 1, Type: ft, Payload: []byte{0x01}}
		data, err := f.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary type 0x%02x: %v", ft, err)
		}
		decoded, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("ReadFrame type 0x%02x: %v", ft, err)
		}
		if decoded.Type != ft {
			t.Fatalf("type mismatch: got 0x%02x, want 0x%02x", decoded.Type, ft)
		}
	}
}

func TestFrameEmptyPayload(t *testing.T) {
	f := Frame{StreamID: 1, Type: FrameOpen, Payload: nil}
	data, err := f.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	decoded, err := ReadFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if len(decoded.Payload) != 0 {
		t.Fatalf("expected empty payload, got %d bytes", len(decoded.Payload))
	}
}

func TestFrameLargePayload(t *testing.T) {
	payload := make([]byte, 65535) // max payload
	for i := range payload {
		payload[i] = byte(i)
	}
	f := Frame{StreamID: 99, Type: FrameData, Payload: payload}
	data, err := f.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary max payload: %v", err)
	}
	decoded, err := ReadFrame(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrame max payload: %v", err)
	}
	if !bytes.Equal(decoded.Payload, payload) {
		t.Fatal("max payload round-trip mismatch")
	}
}

func TestFrameSequenceNumbers(t *testing.T) {
	for _, seq := range []uint32{0, 1, 0xFFFFFFFF} {
		f := Frame{StreamID: 1, Type: FrameData, Sequence: seq, Payload: []byte{1}}
		data, _ := f.MarshalBinary()
		decoded, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("ReadFrame seq %d: %v", seq, err)
		}
		if decoded.Sequence != seq {
			t.Fatalf("seq: got %d, want %d", decoded.Sequence, seq)
		}
	}
}
