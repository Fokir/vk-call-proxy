// Package provider defines interfaces for pluggable call services (VK, MAX, etc.)
// that provide TURN credentials and signaling for relay-to-relay VPN tunneling.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// ErrDisconnectReceived is returned when a disconnect signal is received
// from a new client while waiting for relay data. The Nonce field identifies
// the requesting session so the server can send an ack.
var ErrDisconnectReceived = errors.New("disconnect signal received")

// DisconnectError wraps ErrDisconnectReceived with the session nonce.
type DisconnectError struct {
	Nonce string
}

func (e *DisconnectError) Error() string { return "disconnect signal received (nonce=" + e.Nonce + ")" }
func (e *DisconnectError) Unwrap() error { return ErrDisconnectReceived }

// TURNServer is a single TURN server address.
type TURNServer struct {
	Host string
	Port string
}

// Credentials holds TURN server access information.
type Credentials struct {
	Username string
	Password string
	Host     string // primary TURN server host (first URL)
	Port     string // primary TURN server port
	Servers  []TURNServer // all available TURN servers (for load distribution)
}

// JoinInfo holds the result of joining a conference/call service,
// including TURN credentials, signaling endpoint, and session metadata.
type JoinInfo struct {
	Credentials Credentials
	WSEndpoint  string // signaling WebSocket endpoint
	ConvID      string // conversation/conference ID
	DeviceIdx   int
}

// SignalMessage represents a decoded signaling message delivered to subscribers.
type SignalMessage struct {
	Type    string // wire-level tag (e.g. "av-conn-new")
	Payload string // base64-encoded payload
	Nonce   string // session nonce (empty for legacy messages)
	Index   int    // connection index (used by punch-ready signals)
}

// SessionEndReason indicates why a signaling session ended.
type SessionEndReason int

const (
	SessionEndHungup     SessionEndReason = iota // remote peer left the call
	SessionEndDisconnect                         // peer sent explicit disconnect
	SessionEndClosed                             // connection closed or context cancelled
)

// RateLimitError is returned by providers when the upstream API throttles requests.
// Code contains the provider-specific error code (e.g. VK codes 6, 9, 14, 29, 1105).
type RateLimitError struct {
	Code    int
	Message string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limit (code %d): %s", e.Code, e.Message)
}

// IsRateLimitError checks whether err is a RateLimitError and returns it.
func IsRateLimitError(err error) (*RateLimitError, bool) {
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return rle, true
	}
	return nil, false
}

// CredentialsProvider fetches TURN credentials for individual allocations.
// Each call may return fresh credentials (e.g. new anonymous identity).
type CredentialsProvider interface {
	FetchCredentials(ctx context.Context) (*Credentials, error)
}

// SessionProvider extends CredentialsProvider with full session info
// needed for relay-to-relay mode (signaling endpoint, etc.).
type SessionProvider interface {
	CredentialsProvider
	FetchJoinInfo(ctx context.Context) (*JoinInfo, error)
}

// SignalingClient abstracts the signaling channel used in relay-to-relay mode
// to exchange TURN relay addresses between peers.
type SignalingClient interface {
	SetKey(token string) error
	SendRelayAddrs(ctx context.Context, addrs []string, role string, nonce string) error
	RecvRelayAddrs(ctx context.Context, skipRole string, nonce string) (addrs []string, role string, recvNonce string, err error)
	SendRelayBatch(ctx context.Context, addrs []string, role string, nonce string, batch int, final bool) error
	RecvRelayBatch(ctx context.Context, skipRole string, filterNonce string) (addrs []string, batch int, final bool, nonce string, err error)
	SendDisconnectReq(ctx context.Context, nonce string) error
	WaitDisconnectAck(ctx context.Context, nonce string) error
	SendDisconnectAck(ctx context.Context, nonce string) error
	SendPunchReady(ctx context.Context, nonce string, index int) error
	PreparePunchWait(ctx context.Context, nonce string, index int) func() error
	WaitPunchReady(ctx context.Context, nonce string, index int) error
	StartPunchDispatcher(ctx context.Context, nonce string)
	StopPunchDispatcher()
	WaitForSessionEnd(ctx context.Context) (SessionEndReason, string)
	SendPayload(ctx context.Context, tag string, data []byte) error
	RecvPayload(ctx context.Context, tag string) ([]byte, error)
	Subscribe(tag string, bufSize int) (<-chan SignalMessage, func())
	Drain()
	DrainAndRoute(ctx context.Context)
	Done() <-chan struct{}
	IsAlive() bool
	PeerID() string
	SendHangup() error
	Close() error
}

// SignalingConnector creates a signaling client from join info.
type SignalingConnector interface {
	ConnectSignaling(ctx context.Context, info *JoinInfo, logger *slog.Logger) (SignalingClient, error)
}

// Service combines all capabilities needed for a call service.
// Implementations: vk.Service, (future) max.Service, etc.
type Service interface {
	Name() string
	SessionProvider
	SignalingConnector
}

// TokenAuthProvider is an optional interface for providers that support
// authenticated token-based credential acquisition (fewer API calls, no rate-limit risk).
type TokenAuthProvider interface {
	FetchJoinInfoWithToken(ctx context.Context, token string) (*JoinInfo, error)
}
