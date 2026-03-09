// Package provider defines interfaces for pluggable call services (VK, MAX, etc.)
// that provide TURN credentials and signaling for relay-to-relay VPN tunneling.
package provider

import (
	"context"
	"log/slog"
)

// Credentials holds TURN server access information.
type Credentials struct {
	Username string
	Password string
	Host     string
	Port     string
}

// JoinInfo holds the result of joining a conference/call service,
// including TURN credentials, signaling endpoint, and session metadata.
type JoinInfo struct {
	Credentials Credentials
	WSEndpoint  string // signaling WebSocket endpoint
	ConvID      string // conversation/conference ID
	DeviceIdx   int
}

// SessionEndReason indicates why a signaling session ended.
type SessionEndReason int

const (
	SessionEndHungup     SessionEndReason = iota // remote peer left the call
	SessionEndDisconnect                         // peer sent explicit disconnect
	SessionEndClosed                             // connection closed or context cancelled
)

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
	SendRelayAddrs(ctx context.Context, addrs []string, role string) error
	RecvRelayAddrs(ctx context.Context, skipRole string) (addrs []string, role string, err error)
	SendDisconnect(ctx context.Context) error
	WaitForSessionEnd(ctx context.Context) SessionEndReason
	SendPayload(ctx context.Context, tag string, data []byte) error
	RecvPayload(ctx context.Context, tag string) ([]byte, error)
	Subscribe(tag string, bufSize int) (<-chan []byte, func())
	Drain()
	DrainAndRoute(ctx context.Context)
	Done() <-chan struct{}
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
