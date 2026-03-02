package mux

import (
	"fmt"
	"io"
	"time"
)

const sessionIDSize = 16

// WriteSessionID sends a 16-byte session UUID as the first data after
// a DTLS handshake. The server uses this to group multiple connections
// from the same client into a single MUX.
func WriteSessionID(conn io.Writer, sessionID [16]byte) error {
	_, err := conn.Write(sessionID[:])
	if err != nil {
		return fmt.Errorf("write session id: %w", err)
	}
	return nil
}

// ReadSessionID reads a 16-byte session UUID from a new connection.
// Applies a 10-second read deadline if the connection supports it.
func ReadSessionID(conn io.Reader) ([16]byte, error) {
	var id [16]byte

	// Set read deadline if supported.
	type deadliner interface {
		SetReadDeadline(time.Time) error
	}
	if d, ok := conn.(deadliner); ok {
		d.SetReadDeadline(time.Now().Add(10 * time.Second))
		defer d.SetReadDeadline(time.Time{})
	}

	if _, err := io.ReadFull(conn, id[:]); err != nil {
		return id, fmt.Errorf("read session id: %w", err)
	}
	return id, nil
}
