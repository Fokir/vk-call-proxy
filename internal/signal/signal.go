// Package signal implements VK WebSocket signaling for relay-to-relay
// address exchange. Both peers (client and server) join the same VK call
// and use custom-data messages to exchange their TURN relay addresses.
//
// On the wire, the entire payload is encrypted with AES-256-GCM when a
// shared token is provided, making custom-data contents opaque to VK.
// Without a token, payloads are base64-encoded with neutral identifiers.
package signal

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
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
	aead       cipher.AEAD // nil = no encryption (base64 only)
}

// SetKey derives an AES-256-GCM key from the shared token.
// When set, SendRelayAddrs encrypts and RecvRelayAddrs decrypts payloads.
func (c *Client) SetKey(token string) error {
	if token == "" {
		return nil
	}
	h := sha256.Sum256([]byte(token))
	block, err := aes.NewCipher(h[:])
	if err != nil {
		return fmt.Errorf("aes cipher: %w", err)
	}
	c.aead, err = cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("gcm: %w", err)
	}
	return nil
}

// seal encrypts plaintext with AES-GCM and returns base64(nonce+ciphertext).
// If no key is set, returns base64(plaintext).
func (c *Client) seal(plaintext []byte) string {
	if c.aead == nil {
		return base64.StdEncoding.EncodeToString(plaintext)
	}
	nonce := make([]byte, c.aead.NonceSize())
	rand.Read(nonce)
	ct := c.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ct)
}

// open decodes base64 and decrypts AES-GCM. If no key is set, just decodes base64.
func (c *Client) open(blob string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return nil, err
	}
	if c.aead == nil {
		return raw, nil
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return c.aead.Open(nil, raw[:ns], raw[ns:], nil)
}

type notification struct {
	Name string          `json:"notification"`
	Data json.RawMessage `json:"data"` // used by custom-data
	Raw  json.RawMessage // the entire message (for participant-joined etc.)
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
	// VK requires additional query parameters for the signaling WebSocket.
	sep := "&"
	if !strings.Contains(wsEndpoint, "?") {
		sep = "?"
	}
	wsEndpoint += sep + "platform=WEB&appVersion=1.1&version=5&device=browser&clientType=PORTAL&deviceIdx=0"

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
	logger.Debug("signaling raw connection message", "raw", string(msg))

	// The first message can be a "connection" notification or an error.
	var envelope struct {
		Type         string `json:"type"`
		Notification string `json:"notification"`
		Error        string `json:"error"`
		Message      string `json:"message"`
		PeerID       struct {
			ID json.Number `json:"id"`
		} `json:"peerId"`
	}
	if err := json.Unmarshal(msg, &envelope); err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse first message (raw=%s): %w", string(msg), err)
	}
	if envelope.Error != "" {
		conn.Close()
		return nil, fmt.Errorf("signaling error: %s: %s", envelope.Error, envelope.Message)
	}
	if envelope.Notification != "connection" {
		conn.Close()
		return nil, fmt.Errorf("expected 'connection' notification, got %q (raw=%s)", envelope.Notification, string(msg))
	}
	c.myPeerID = envelope.PeerID.ID.String()
	logger.Info("signaling connected", "peer_id", c.myPeerID)

	go c.readLoop()

	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		_, msg, err := c.conn.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				c.logger.Warn("signaling websocket closed", "code", ce.Code, "reason", ce.Text)
			} else {
				c.logger.Warn("signaling read error", "err", err)
			}
			return
		}

		// VK sends text "ping" for keepalive.
		if string(msg) == "ping" {
			c.mu.Lock()
			err := c.conn.WriteMessage(websocket.TextMessage, []byte("pong"))
			c.mu.Unlock()
			if err != nil {
				c.logger.Warn("signaling pong write failed", "err", err)
			}
			continue
		}

		c.logger.Debug("signaling recv", "raw", string(msg))

		var notif notification
		if err := json.Unmarshal(msg, &notif); err != nil {
			c.logger.Debug("signaling parse error", "raw", string(msg), "err", err)
			continue
		}
		notif.Raw = msg // preserve the entire message for top-level fields

		if notif.Name != "" {
			c.logger.Info("signaling notification", "name", notif.Name)
			select {
			case c.incoming <- notif:
			default:
				c.logger.Warn("signaling incoming buffer full, dropping")
			}
		}
	}
}

// WaitForPeer blocks until a "participant-joined" notification arrives
// from a peer that is not one of our own TURN allocations. skipCount
// specifies how many participant-joined notifications to skip (typically
// equal to the number of TURN allocations created before calling this).
func (c *Client) WaitForPeer(ctx context.Context, skipCount int) (string, error) {
	skipped := 0
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.done:
			return "", fmt.Errorf("signaling connection closed")
		case notif := <-c.incoming:
			if notif.Name == "participant-joined" {
				// VK puts participant data at the top level, not inside "data".
				var data struct {
					Participant struct {
						PeerID struct {
							ID json.Number `json:"id"`
						} `json:"peerId"`
					} `json:"participant"`
				}
				if err := json.Unmarshal(notif.Raw, &data); err != nil {
					c.logger.Warn("parse participant-joined", "err", err)
					continue
				}
				peerID := data.Participant.PeerID.ID.String()
				if skipped < skipCount {
					skipped++
					c.logger.Debug("skipping own participant-joined", "peer_id", peerID, "skipped", skipped, "of", skipCount)
					continue
				}
				c.remotePeer = peerID
				c.logger.Info("remote peer joined", "peer_id", peerID)
				return peerID, nil
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
	blob := c.seal(innerJSON)

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
				// Try notif.Data first; fall back to top-level "data" from Raw.
				dataField := notif.Data
				if len(dataField) == 0 || string(dataField) == "null" {
					var raw struct {
						Data json.RawMessage `json:"data"`
					}
					if err := json.Unmarshal(notif.Raw, &raw); err == nil && len(raw.Data) > 0 {
						dataField = raw.Data
					}
				}
				c.logger.Debug("custom-data received", "data_len", len(dataField))

				var blob string
				if err := json.Unmarshal(dataField, &blob); err != nil {
					c.logger.Debug("custom-data not a string, skip", "data", string(dataField), "err", err)
					continue
				}
				innerJSON, err := c.open(blob)
				if err != nil {
					c.logger.Debug("custom-data decode/decrypt failed", "err", err)
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
