package tunnel

import (
	"log/slog"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// SlotState represents the current state of a CallSlot.
type SlotState int

const (
	SlotIdle         SlotState = iota // not started
	SlotConnecting                    // joining call + signaling
	SlotAllocating                    // TURN allocation in progress
	SlotActive                        // connections live in MUX
	SlotReconnecting                  // reconnect in progress
	SlotDead                          // permanently failed (context cancelled)
)

func (s SlotState) String() string {
	switch s {
	case SlotIdle:
		return "idle"
	case SlotConnecting:
		return "connecting"
	case SlotAllocating:
		return "allocating"
	case SlotActive:
		return "active"
	case SlotReconnecting:
		return "reconnecting"
	case SlotDead:
		return "dead"
	default:
		return "unknown"
	}
}

// PoolConfig configures the CallPool.
type PoolConfig struct {
	Services         []provider.Service // one per call link
	ConnsPerCall     int                // --n flag (default 4)
	UseTCP           bool               // TCP vs UDP for TURN
	AuthToken        string             // VPN auth token (shared)
	VKTokens         []string           // VK account tokens for fast allocation
	Fingerprint      []byte             // server DTLS cert fingerprint (client only)
	SlotConnectDelay time.Duration      // delay between sequential slot connects (0 = default 3s)
	Logger           *slog.Logger
}

// SlotStatus is a snapshot of one slot's health.
type SlotStatus struct {
	Index      int
	State      SlotState
	ActiveConn int   // connections currently in MUX
	TotalConn  int   // connections ever added
	LastError  error
	Link       string
}

// Reconnect backoff constants.
const (
	reconnectInitDelay = 3 * time.Second
	reconnectMaxDelay  = 60 * time.Second
	slotConnectDelay   = 3 * time.Second // delay between sequential slot connects
)
