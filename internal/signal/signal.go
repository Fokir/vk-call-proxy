// Package signal implements VK WebSocket signaling for relay-to-relay
// address exchange. Both peers (client and server) join the same VK call
// and use custom-data messages to exchange their TURN relay addresses.
//
// On the wire, messages use neutral identifiers to avoid revealing intent:
//   - type "av-sync" (instead of "vpn-relay")
//   - roles "pub"/"sub" (instead of "server"/"client")
//   - addresses encoded in base64
package signal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

const (
	// Wire-level identifiers intentionally neutral.
	wireType       = "av-sync"
	wireRoleServer = "pub"
	wireRoleClient = "sub"
)

// Client manages a VK signaling WebSocket connection.
type Client struct {
	conn       *websocket.Conn
	myPeerID   string
	remotePeer string
	logger     *slog.Logger
	mu         sync.Mutex
	seq        int
	incoming   chan notification
	done       chan struct{}
}

type notification struct {
	Name string          `json:"notification"`
	Data json.RawMessage `json:"data"`
}

type command struct {
	Command  string      `json:"command"`
	Sequence int         `json:"sequence"`
	Data     interface{} `json:"data,omitempty"`
}

type relayData struct {
	Type    string `json:"type"`
	Payload string `json:"payload"` // base64-encoded comma-separated addresses
	Mode    string `json:"mode"`    // "pub" or "sub"
}

func encodeAddrs(addrs []string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(addrs, ",")))
}

func decodeAddrs(payload string) ([]string, error) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(raw), ","), nil
}

func toWireRole(role string) string {
	if role == "server" {
		return wireRoleServer
	}
	return wireRoleClient
}

func fromWireRole(mode string) string {
	if mode == wireRoleServer {
		return "server"
	}
	return "client"
}

// Connect dials the VK WebSocket endpoint and starts the read loop.
// The first message received is a "connection" notification with our peer ID.
func Connect(ctx context.Context, wsEndpoint string, logger *slog.Logger) (*Client, error) {
	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, wsEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}

	c := &Client{
		conn:     conn,
		logger:   logger,
		incoming: make(chan notification, 64),
		done:     make(chan struct{}),
	}

	// Read the initial "connection" notification to get our peer ID.
	_, msg, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read connection notification: %w", err)
	}

	var connNotif struct {
		Notification string `json:"notification"`
		PeerID       string `json:"peerId"`
	}
	if err := json.Unmarshal(msg, &connNotif); err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse connection notification: %w", err)
	}
	c.myPeerID = connNotif.PeerID
	logger.Info("signaling connected", "peer_id", c.myPeerID)

	go c.readLoop()

	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			c.logger.Debug("signaling read error", "err", err)
			return
		}

		// VK sends text "ping" for keepalive.
		if string(msg) == "ping" {
			c.mu.Lock()
			c.conn.WriteMessage(websocket.TextMessage, []byte("pong"))
			c.mu.Unlock()
			continue
		}

		var notif notification
		if err := json.Unmarshal(msg, &notif); err != nil {
			c.logger.Debug("signaling parse error", "raw", string(msg), "err", err)
			continue
		}

		if notif.Name != "" {
			select {
			case c.incoming <- notif:
			default:
				c.logger.Warn("signaling incoming buffer full, dropping")
			}
		}
	}
}

// WaitForPeer blocks until a "participant-joined" notification arrives.
func (c *Client) WaitForPeer(ctx context.Context) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.done:
			return "", fmt.Errorf("signaling connection closed")
		case notif := <-c.incoming:
			if notif.Name == "participant-joined" {
				var data struct {
					PeerID string `json:"peerId"`
				}
				if err := json.Unmarshal(notif.Data, &data); err != nil {
					c.logger.Warn("parse participant-joined", "err", err)
					continue
				}
				c.remotePeer = data.PeerID
				c.logger.Info("remote peer joined", "peer_id", data.PeerID)
				return data.PeerID, nil
			}
		}
	}
}

// SendRelayAddrs sends our TURN relay addresses to the remote peer
// via a custom-data command. The entire payload is base64-encoded so
// VK only sees an opaque string in the data field.
func (c *Client) SendRelayAddrs(ctx context.Context, addrs []string, role string) error {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()

	inner := relayData{
		Type:    wireType,
		Payload: encodeAddrs(addrs),
		Mode:    toWireRole(role),
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return fmt.Errorf("marshal inner: %w", err)
	}
	blob := base64.StdEncoding.EncodeToString(innerJSON)

	cmd := command{
		Command:  "custom-data",
		Sequence: seq,
		Data:     blob,
	}

	msg, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		return fmt.Errorf("write command: %w", err)
	}

	c.logger.Info("sent relay addresses", "count", len(addrs), "role", role)
	return nil
}

// RecvRelayAddrs waits for a custom-data notification containing
// the remote peer's relay addresses. The data field is a base64 blob
// wrapping the inner JSON structure.
func (c *Client) RecvRelayAddrs(ctx context.Context) (addrs []string, role string, err error) {
	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-c.done:
			return nil, "", fmt.Errorf("signaling connection closed")
		case notif := <-c.incoming:
			if notif.Name == "custom-data" {
				// data is a JSON string (base64 blob).
				var blob string
				if err := json.Unmarshal(notif.Data, &blob); err != nil {
					c.logger.Debug("custom-data not a string, skip", "err", err)
					continue
				}
				innerJSON, err := base64.StdEncoding.DecodeString(blob)
				if err != nil {
					c.logger.Debug("custom-data base64 decode failed", "err", err)
					continue
				}
				var data relayData
				if err := json.Unmarshal(innerJSON, &data); err != nil {
					c.logger.Warn("parse inner data", "err", err)
					continue
				}
				if data.Type != wireType {
					continue
				}
				addrs, err := decodeAddrs(data.Payload)
				if err != nil {
					c.logger.Warn("decode relay addrs", "err", err)
					continue
				}
				role := fromWireRole(data.Mode)
				c.logger.Info("received relay addresses", "count", len(addrs), "role", role)
				return addrs, role, nil
			}
		}
	}
}

// PeerID returns our peer ID assigned by the signaling server.
func (c *Client) PeerID() string {
	return c.myPeerID
}

// Close shuts down the WebSocket connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
