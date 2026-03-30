// VK WebSocket signaling for relay-to-relay address exchange.
// Both peers (client and server) join the same VK call and use
// custom-data messages to exchange their TURN relay addresses.
//
// On the wire, the entire payload is encrypted with AES-256-GCM when a
// shared token is provided, making custom-data contents opaque to VK.
// Without a token, payloads are base64-encoded with neutral identifiers.
package vk

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
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/gorilla/websocket"
)

const (
	// Wire-level identifiers intentionally neutral.
	wireType          = "av-sync"
	wireDisconnect    = "av-reset"     // explicit session teardown signal
	wireDisconnectAck = "av-reset-ack" // acknowledgement of disconnect
	wirePunchReady    = "av-punch"     // punch-ready signal per connection index
	wireRoleServer    = "pub"
	wireRoleClient    = "sub"
)

// Compile-time check: SignalingClient implements provider.SignalingClient.
var _ provider.SignalingClient = (*SignalingClient)(nil)

// SignalingClient manages a VK signaling WebSocket connection.
type SignalingClient struct {
	conn       *websocket.Conn
	myPeerID   string
	remotePeer string
	logger     *slog.Logger
	mu         sync.Mutex
	seq        int
	incoming   chan notification
	done       chan struct{}
	aead       cipher.AEAD // nil = no encryption (base64 only)

	subsMu sync.Mutex
	subs   map[string]chan provider.SignalMessage // tag -> subscriber channel

	punchDisp *punchDispatcher // initialized by StartPunchDispatcher
}

// SetKey derives an AES-256-GCM key from the shared token.
// When set, SendRelayAddrs encrypts and RecvRelayAddrs decrypts payloads.
func (c *SignalingClient) SetKey(token string) error {
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
func (c *SignalingClient) seal(plaintext []byte) string {
	if c.aead == nil {
		return base64.StdEncoding.EncodeToString(plaintext)
	}
	nonce := make([]byte, c.aead.NonceSize())
	rand.Read(nonce)
	ct := c.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ct)
}

// open decodes base64 and decrypts AES-GCM. If no key is set, just decodes base64.
func (c *SignalingClient) open(blob string) ([]byte, error) {
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
	Payload string `json:"payload"`           // base64-encoded comma-separated addresses
	Mode    string `json:"mode"`              // "pub" or "sub"
	Nonce   string `json:"nonce,omitempty"`   // session nonce for filtering ghost messages
	Index   int    `json:"index,omitempty"`   // connection index for punch-ready signals
	Batch   int    `json:"batch,omitempty"`   // batch sequence number for gradual allocation
	Final   bool   `json:"final,omitempty"`   // true if this is the last batch
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

// ConnectSignaling dials the VK WebSocket endpoint and starts the read loop.
// The first message received is a "connection" notification with our peer ID.
// deviceIdx differentiates concurrent connections from the same anonymous user;
// VK kicks participants with the same deviceIdx via "ANOTHER_DEVICE".
func ConnectSignaling(ctx context.Context, wsEndpoint string, deviceIdx int, logger *slog.Logger) (*SignalingClient, error) {
	// VK requires additional query parameters for the signaling WebSocket.
	sep := "&"
	if !strings.Contains(wsEndpoint, "?") {
		sep = "?"
	}
	wsEndpoint += sep + joinParamsWithDeviceIdx(deviceIdx)

	dialer := websocket.Dialer{}
	conn, _, err := dialer.DialContext(ctx, wsEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}

	c := &SignalingClient{
		conn:     conn,
		logger:   logger,
		incoming: make(chan notification, 256),
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

func (c *SignalingClient) readLoop() {
	defer close(c.done)
	// Read deadline detects stale connections: VK sends pings every ~30s,
	// so 90s without any message means the connection is dead.
	const readTimeout = 90 * time.Second
	for {
		c.conn.SetReadDeadline(time.Now().Add(readTimeout))
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
func (c *SignalingClient) WaitForPeer(ctx context.Context, skipCount int) (string, error) {
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

// sendInner marshals a relayData, encrypts, and sends it as a custom-data command.
func (c *SignalingClient) sendInner(ctx context.Context, inner relayData) error {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()

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
	return nil
}

// SendRelayAddrs sends our TURN relay addresses to the remote peer
// via a custom-data command. The entire payload is base64-encoded so
// VK only sees an opaque string in the data field.
func (c *SignalingClient) SendRelayAddrs(ctx context.Context, addrs []string, role string, nonce string) error {
	inner := relayData{
		Type:    wireType,
		Payload: encodeAddrs(addrs),
		Mode:    toWireRole(role),
		Nonce:   nonce,
	}
	if err := c.sendInner(ctx, inner); err != nil {
		return err
	}
	c.logger.Info("sent relay addresses", "count", len(addrs), "role", role, "nonce", nonce)
	return nil
}

// SendRelayBatch sends a batch of TURN relay addresses with batch metadata.
func (c *SignalingClient) SendRelayBatch(ctx context.Context, addrs []string, role string, nonce string, batch int, final bool) error {
	inner := relayData{
		Type:    wireType,
		Payload: encodeAddrs(addrs),
		Mode:    toWireRole(role),
		Nonce:   nonce,
		Batch:   batch,
		Final:   final,
	}
	if err := c.sendInner(ctx, inner); err != nil {
		return err
	}
	c.logger.Info("sent relay batch", "count", len(addrs), "role", role, "batch", batch, "final", final)
	return nil
}

// recvRelayData waits for the next relay data message, applying role/nonce filters.
func (c *SignalingClient) recvRelayData(ctx context.Context, skipRole string, filterNonce string) (*relayData, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, fmt.Errorf("signaling connection closed")
		case notif := <-c.incoming:
			if notif.Name != "custom-data" {
				continue
			}
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
			if data.Type == wireDisconnect {
				c.logger.Info("disconnect signal received during relay exchange", "nonce", data.Nonce)
				return nil, &provider.DisconnectError{Nonce: data.Nonce}
			}
			if data.Type != wireType {
				continue
			}
			if filterNonce != "" && data.Nonce != filterNonce {
				c.logger.Debug("skipping relay data with wrong nonce", "got", data.Nonce, "want", filterNonce)
				continue
			}
			role := fromWireRole(data.Mode)
			if skipRole != "" && role == skipRole {
				c.logger.Debug("skipping own echoed relay data", "role", role)
				continue
			}
			// Remember the sender's participantId so we can filter
			// hungup notifications later (ignore anonymous TURN users).
			c.setRemotePeerFromNotif(notif)
			return &data, nil
		}
	}
}

// RecvRelayAddrs waits for a custom-data notification containing
// the remote peer's relay addresses. The data field is a base64 blob
// wrapping the inner JSON structure. Messages whose role matches
// skipRole are silently discarded (filters out our own echoed broadcasts).
// When filterNonce is non-empty, only messages with a matching nonce are accepted.
// Returns the received nonce as the third string value.
func (c *SignalingClient) RecvRelayAddrs(ctx context.Context, skipRole string, filterNonce string) (addrs []string, role string, recvNonce string, err error) {
	data, err := c.recvRelayData(ctx, skipRole, filterNonce)
	if err != nil {
		return nil, "", "", err
	}
	addrs, err = decodeAddrs(data.Payload)
	if err != nil {
		return nil, "", "", fmt.Errorf("decode relay addrs: %w", err)
	}
	role = fromWireRole(data.Mode)
	c.logger.Info("received relay addresses", "count", len(addrs), "role", role, "nonce", data.Nonce)
	return addrs, role, data.Nonce, nil
}

// RecvRelayBatch waits for a batch relay data message and returns
// the addresses, batch number, and whether this is the final batch.
func (c *SignalingClient) RecvRelayBatch(ctx context.Context, skipRole string, filterNonce string) ([]string, int, bool, string, error) {
	data, err := c.recvRelayData(ctx, skipRole, filterNonce)
	if err != nil {
		return nil, 0, false, "", err
	}
	addrs, err := decodeAddrs(data.Payload)
	if err != nil {
		return nil, 0, false, "", fmt.Errorf("decode relay batch: %w", err)
	}
	c.logger.Info("received relay batch", "count", len(addrs), "batch", data.Batch, "final", data.Final)
	return addrs, data.Batch, data.Final, data.Nonce, nil
}

// Drain discards all buffered notifications from the incoming channel.
func (c *SignalingClient) Drain() {
	for {
		select {
		case <-c.incoming:
		default:
			return
		}
	}
}

// WaitForHungup blocks until a "hungup" notification is received,
// the signaling connection closes, or ctx is cancelled. It drains
// other notifications to prevent buffer overflow.
func (c *SignalingClient) WaitForHungup(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case notif := <-c.incoming:
			if notif.Name == "hungup" {
				return
			}
		}
	}
}

// SendDisconnectReq sends a disconnect request with a session nonce.
func (c *SignalingClient) SendDisconnectReq(ctx context.Context, nonce string) error {
	return c.sendInner(ctx, relayData{Type: wireDisconnect, Nonce: nonce})
}

// SendDisconnectAck sends a disconnect acknowledgement with a session nonce.
func (c *SignalingClient) SendDisconnectAck(ctx context.Context, nonce string) error {
	return c.sendInner(ctx, relayData{Type: wireDisconnectAck, Nonce: nonce})
}

// WaitDisconnectAck subscribes to disconnect-ack messages and waits for one
// matching the given nonce. Returns nil on match, ctx.Err() on timeout.
func (c *SignalingClient) WaitDisconnectAck(ctx context.Context, nonce string) error {
	ch, unsub := c.Subscribe(wireDisconnectAck, 4)
	defer unsub()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return fmt.Errorf("signaling connection closed")
		case msg := <-ch:
			if msg.Nonce == nonce {
				return nil
			}
		}
	}
}

// SendPunchReady signals that a specific connection index is ready for punching.
func (c *SignalingClient) SendPunchReady(ctx context.Context, nonce string, index int) error {
	return c.sendInner(ctx, relayData{Type: wirePunchReady, Nonce: nonce, Index: index})
}

// punchDispatcher fans out "av-punch" messages to per-index waiters.
type punchDispatcher struct {
	mu      sync.Mutex
	waiters map[int]chan struct{} // index -> signal channel
	cancel  context.CancelFunc
}

// initPunchDispatcher starts a goroutine that subscribes to wirePunchReady
// and dispatches signals to per-index waiters matching the given nonce.
func (c *SignalingClient) initPunchDispatcher(ctx context.Context, nonce string) *punchDispatcher {
	dctx, cancel := context.WithCancel(ctx)
	pd := &punchDispatcher{
		waiters: make(map[int]chan struct{}),
		cancel:  cancel,
	}
	ch, unsub := c.Subscribe(wirePunchReady, 32)
	go func() {
		defer unsub()
		for {
			select {
			case <-dctx.Done():
				return
			case <-c.done:
				return
			case msg := <-ch:
				if msg.Nonce != nonce {
					continue
				}
				pd.mu.Lock()
				if w, ok := pd.waiters[msg.Index]; ok {
					select {
					case w <- struct{}{}:
					default:
					}
				}
				pd.mu.Unlock()
			}
		}
	}()
	return pd
}

func (pd *punchDispatcher) Close() {
	pd.cancel()
}

func (pd *punchDispatcher) wait(ctx context.Context, index int) error {
	ch := make(chan struct{}, 1)
	pd.mu.Lock()
	pd.waiters[index] = ch
	pd.mu.Unlock()
	defer func() {
		pd.mu.Lock()
		delete(pd.waiters, index)
		pd.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// StartPunchDispatcher initializes a shared punch dispatcher that subscribes
// once to "av-punch" and fans out messages to per-index waiters. Must be
// called before launching concurrent WaitPunchReady goroutines.
func (c *SignalingClient) StartPunchDispatcher(ctx context.Context, nonce string) {
	c.punchDisp = c.initPunchDispatcher(ctx, nonce)
}

// StopPunchDispatcher stops the punch dispatcher goroutine.
func (c *SignalingClient) StopPunchDispatcher() {
	if c.punchDisp != nil {
		c.punchDisp.Close()
		c.punchDisp = nil
	}
}

// PreparePunchWait registers a waiter for the given index BEFORE sending
// SendPunchReady to avoid the race where the peer's response arrives before
// the waiter is registered. Returns a function that blocks until the signal.
func (c *SignalingClient) PreparePunchWait(ctx context.Context, nonce string, index int) func() error {
	pd := c.punchDisp
	if pd == nil {
		return func() error {
			<-ctx.Done()
			return ctx.Err()
		}
	}
	// Register waiter NOW, before peer can respond.
	ch := make(chan struct{}, 1)
	pd.mu.Lock()
	pd.waiters[index] = ch
	pd.mu.Unlock()
	return func() error {
		defer func() {
			pd.mu.Lock()
			delete(pd.waiters, index)
			pd.mu.Unlock()
		}()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			return nil
		}
	}
}

// WaitPunchReady blocks until a punch-ready signal for the given index
// and nonce is received. Uses the shared punch dispatcher if started,
// otherwise falls back to waiting on the context (old peer without dispatcher).
func (c *SignalingClient) WaitPunchReady(ctx context.Context, nonce string, index int) error {
	wait := c.PreparePunchWait(ctx, nonce, index)
	return wait()
}

// WaitForSessionEnd blocks until one of the following occurs:
//   - "hungup" notification from the remote peer -> SessionEndHungup
//   - explicit disconnect signal (custom-data "av-reset") -> SessionEndDisconnect
//   - connection close or ctx cancellation -> SessionEndClosed
//
// Hungup notifications from other participants (e.g. anonymous TURN
// identities) are ignored -- only a hungup matching remotePeer triggers
// session end.
// It also routes custom-data to subscribers to keep reconnect signaling alive.
func (c *SignalingClient) WaitForSessionEnd(ctx context.Context) (provider.SessionEndReason, string) {
	for {
		select {
		case <-ctx.Done():
			return provider.SessionEndClosed, ""
		case <-c.done:
			return provider.SessionEndClosed, ""
		case notif := <-c.incoming:
			if notif.Name == "hungup" {
				if c.isRemotePeerHungup(notif) {
					return provider.SessionEndHungup, ""
				}
				continue
			}
			if notif.Name == "custom-data" {
				// Route to subscribers first (reconnect signals, etc.).
				if c.routeToSubscriber(notif) {
					continue
				}
				if nonce, ok := c.isDisconnectSignal(notif); ok {
					return provider.SessionEndDisconnect, nonce
				}
			}
		}
	}
}

// setRemotePeerFromNotif extracts participantId from a VK notification
// and stores it as the remote peer identity for hungup filtering.
func (c *SignalingClient) setRemotePeerFromNotif(notif notification) {
	var data struct {
		ParticipantID json.Number `json:"participantId"`
	}
	if err := json.Unmarshal(notif.Raw, &data); err != nil {
		return
	}
	pid := data.ParticipantID.String()
	if pid == "" || pid == "0" {
		return
	}
	c.mu.Lock()
	c.remotePeer = pid
	c.mu.Unlock()
	c.logger.Info("remote peer identified", "participant_id", pid)
}

// isRemotePeerHungup checks if the hungup notification came from the
// remote peer we're paired with. Returns false for hungups from other
// anonymous TURN participants sharing the same VK conference.
func (c *SignalingClient) isRemotePeerHungup(notif notification) bool {
	var data struct {
		ParticipantID json.Number `json:"participantId"`
	}
	if err := json.Unmarshal(notif.Raw, &data); err != nil {
		c.logger.Warn("parse hungup participantId", "err", err)
		return true // fail-open: treat unparseable hungup as remote
	}
	pid := data.ParticipantID.String()
	if pid == "" || pid == "0" {
		return true // no participant info — assume remote
	}
	c.mu.Lock()
	remote := c.remotePeer
	c.mu.Unlock()
	if remote == "" {
		return true // no remote peer set yet — assume remote
	}
	if pid == remote {
		c.logger.Info("remote peer hungup", "participant_id", pid)
		return true
	}
	c.logger.Debug("ignoring hungup from non-remote peer", "participant_id", pid, "remote", remote)
	return false
}

// DrainAndRoute reads from incoming and routes custom-data to subscribers
// until ctx is cancelled or the connection closes. Use after WaitForSessionEnd
// returns SessionEndHungup to keep routing reconnect signals.
func (c *SignalingClient) DrainAndRoute(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case notif, ok := <-c.incoming:
			if !ok {
				return
			}
			if notif.Name == "custom-data" {
				c.routeToSubscriber(notif)
			}
		}
	}
}

// isDisconnectSignal checks if a custom-data notification contains
// an explicit disconnect signal ("av-reset"). Returns the nonce and true if found.
func (c *SignalingClient) isDisconnectSignal(notif notification) (string, bool) {
	dataField := notif.Data
	if len(dataField) == 0 || string(dataField) == "null" {
		var raw struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(notif.Raw, &raw); err == nil && len(raw.Data) > 0 {
			dataField = raw.Data
		}
	}
	var blob string
	if err := json.Unmarshal(dataField, &blob); err != nil {
		return "", false
	}
	innerJSON, err := c.open(blob)
	if err != nil {
		return "", false
	}
	var data relayData
	if err := json.Unmarshal(innerJSON, &data); err != nil {
		return "", false
	}
	if data.Type == wireDisconnect {
		return data.Nonce, true
	}
	return "", false
}

// SendPayload sends arbitrary data with a given tag via custom-data command.
// Tag distinguishes payload types ("sdp-offer", "sdp-answer", "ice", "sync", etc.).
func (c *SignalingClient) SendPayload(ctx context.Context, tag string, data []byte) error {
	inner := relayData{
		Type:    tag,
		Payload: base64.StdEncoding.EncodeToString(data),
	}
	if err := c.sendInner(ctx, inner); err != nil {
		return err
	}
	c.logger.Debug("sent payload", "tag", tag, "size", len(data))
	return nil
}

// RecvPayload waits for a custom-data notification with the given tag
// and returns the decoded payload. Non-matching custom-data messages are skipped.
func (c *SignalingClient) RecvPayload(ctx context.Context, tag string) ([]byte, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, fmt.Errorf("signaling connection closed")
		case notif := <-c.incoming:
			if notif.Name != "custom-data" {
				continue
			}
			dataField := notif.Data
			if len(dataField) == 0 || string(dataField) == "null" {
				var raw struct {
					Data json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(notif.Raw, &raw); err == nil && len(raw.Data) > 0 {
					dataField = raw.Data
				}
			}
			var blob string
			if err := json.Unmarshal(dataField, &blob); err != nil {
				continue
			}
			innerJSON, err := c.open(blob)
			if err != nil {
				continue
			}
			var data relayData
			if err := json.Unmarshal(innerJSON, &data); err != nil {
				continue
			}
			if data.Type != tag {
				continue
			}
			payload, err := base64.StdEncoding.DecodeString(data.Payload)
			if err != nil {
				c.logger.Warn("decode payload", "tag", tag, "err", err)
				continue
			}
			return payload, nil
		}
	}
}

// Subscribe registers a subscriber for custom-data messages with the
// given tag. Returns a receive-only channel and an unsubscribe function.
// Only one subscriber per tag is supported; duplicate tags overwrite.
func (c *SignalingClient) Subscribe(tag string, bufSize int) (<-chan provider.SignalMessage, func()) {
	ch := make(chan provider.SignalMessage, bufSize)
	c.subsMu.Lock()
	if c.subs == nil {
		c.subs = make(map[string]chan provider.SignalMessage)
	}
	c.subs[tag] = ch
	c.subsMu.Unlock()
	return ch, func() {
		c.subsMu.Lock()
		if c.subs[tag] == ch {
			delete(c.subs, tag)
		}
		c.subsMu.Unlock()
	}
}

// extractTag decodes a custom-data notification and returns the parsed relayData.
func (c *SignalingClient) extractTag(notif notification) (string, relayData, bool) {
	dataField := notif.Data
	if len(dataField) == 0 || string(dataField) == "null" {
		var raw struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(notif.Raw, &raw); err == nil && len(raw.Data) > 0 {
			dataField = raw.Data
		}
	}
	var blob string
	if err := json.Unmarshal(dataField, &blob); err != nil {
		return "", relayData{}, false
	}
	innerJSON, err := c.open(blob)
	if err != nil {
		return "", relayData{}, false
	}
	var data relayData
	if err := json.Unmarshal(innerJSON, &data); err != nil {
		return "", relayData{}, false
	}
	return data.Type, data, true
}

// routeToSubscriber checks if a custom-data notification matches any
// subscriber. If so, it sends the parsed message and returns true.
func (c *SignalingClient) routeToSubscriber(notif notification) bool {
	tag, data, ok := c.extractTag(notif)
	if !ok {
		return false
	}
	c.subsMu.Lock()
	ch, exists := c.subs[tag]
	c.subsMu.Unlock()
	if !exists {
		return false
	}
	msg := provider.SignalMessage{
		Type:    data.Type,
		Payload: data.Payload,
		Nonce:   data.Nonce,
		Index:   data.Index,
	}
	select {
	case ch <- msg:
	default:
		c.logger.Warn("subscriber buffer full", "tag", tag)
	}
	return true
}

// Done returns a channel that is closed when the signaling WebSocket
// connection dies (read loop exits). Use this to detect signaling death
// without blocking on WaitForHungup.
func (c *SignalingClient) Done() <-chan struct{} {
	return c.done
}

// IsAlive reports whether the signaling WebSocket connection is still active.
func (c *SignalingClient) IsAlive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// PeerID returns our peer ID assigned by the signaling server.
func (c *SignalingClient) PeerID() string {
	return c.myPeerID
}

// StartSignalingKeepAlive starts the periodic keepalive ticker to prevent VK
// from kicking us as idle. Sends transmit-data (fake SDP) and
// change-media-settings every 30s to simulate call activity.
// NOTE: accept-call is now sent earlier, immediately after SetKey, before the
// disconnect handshake. This method only starts the periodic ticker.
func (c *SignalingClient) StartSignalingKeepAlive(ctx context.Context, logger *slog.Logger) {
	// Minimal SDP offer to make VK think we're negotiating WebRTC.
	fakeSDP := `{"type":"offer","sdp":"v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\na=group:BUNDLE 0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\nc=IN IP4 0.0.0.0\r\na=mid:0\r\na=sendrecv\r\na=rtpmap:111 opus/48000/2\r\n"}`

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-ticker.C:
				if err := c.sendCommand("transmit-data", fakeSDP); err != nil {
					logger.Debug("transmit-data keepalive failed", "err", err)
					return
				}
				if err := c.sendCommand("change-media-settings", nil); err != nil {
					logger.Debug("change-media-settings keepalive failed", "err", err)
					return
				}
			}
		}
	}()
}

// sendCommand sends an arbitrary VK signaling command.
func (c *SignalingClient) sendCommand(cmdName string, data interface{}) error {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()

	type rawCmd struct {
		Command  string      `json:"command"`
		Sequence int         `json:"sequence"`
		Data     interface{} `json:"data,omitempty"`
	}
	cmd := rawCmd{Command: cmdName, Sequence: seq, Data: data}
	msg, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", cmdName, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, msg)
}

// SendAcceptCall sends an "accept-call" command to VK signaling.
// This tells VK that the participant has "accepted the call" and moves
// it through the call state machine (JOIN→START). Without this, VK
// considers the participant idle and kicks it after ~60-120 seconds.
func (c *SignalingClient) SendAcceptCall() error {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()

	type mediaSettings struct {
		IsAudioEnabled              bool `json:"isAudioEnabled"`
		IsVideoEnabled              bool `json:"isVideoEnabled"`
		IsScreenSharingEnabled      bool `json:"isScreenSharingEnabled"`
		IsFastScreenSharingEnabled  bool `json:"isFastScreenSharingEnabled"`
		IsAudioSharingEnabled       bool `json:"isAudioSharingEnabled"`
		IsAnimojiEnabled            bool `json:"isAnimojiEnabled"`
	}
	type acceptCallCmd struct {
		Command       string        `json:"command"`
		Sequence      int           `json:"sequence"`
		MediaSettings mediaSettings `json:"mediaSettings"`
	}

	cmd := acceptCallCmd{
		Command:  "accept-call",
		Sequence: seq,
		MediaSettings: mediaSettings{
			IsAudioEnabled: true,
			IsVideoEnabled: false,
		},
	}

	msg, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal accept-call: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, msg)
}

// SendHangup sends a "hangup" command to VK signaling to explicitly
// leave the call. This removes the anonymous participant from the call
// instead of leaving a ghost user until VK's idle timeout fires.
func (c *SignalingClient) SendHangup() error {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()

	cmd := command{
		Command:  "hangup",
		Sequence: seq,
	}
	msg, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal hangup: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, msg)
}

// Close sends a hangup to leave the VK call, then shuts down the WebSocket.
func (c *SignalingClient) Close() error {
	_ = c.SendHangup() // best-effort: don't fail Close on hangup error
	return c.conn.Close()
}
