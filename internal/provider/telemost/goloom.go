package telemost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

// Participant holds metadata about a conference participant.
type Participant struct {
	ID             string
	Name           string
	Description    string
	SendVideo      bool
	DisconnectedAt int64
}

// IsLive returns true if the participant is connected and sending video.
func (p Participant) IsLive() bool {
	return p.DisconnectedAt == 0 && p.SendVideo
}

// GoloomClient manages the WebSocket connection to the Goloom media server
// and handles the signaling protocol for WebRTC session establishment.
type GoloomClient struct {
	conn   *websocket.Conn
	logger *slog.Logger

	mu sync.Mutex // protects writes to conn

	// Channels for SDP exchange.
	pubAnswer  chan string // publisherSdpAnswer
	pubError   chan string // error ack for publisherSdpOffer
	subOffer   chan string // subscriberSdpOffer (may receive multiple)
	serverICE  chan iceCandidate
	serverHelloData *serverHello

	// Participant tracking from updateDescription + upsertDescription.
	peerUpdates   chan []Participant // all participant updates (initial + incremental)
	upsertUpdates chan []Participant // only incremental upserts (new joiners)

	// New peer discovery from slotsConfig (for server-side pairing).
	knownPeers       sync.Map     // map[string]bool — IDs we've already seen
	newPeerCh        chan string   // newly discovered participant IDs
	discoveryEnabled atomic.Bool   // gate: parseSlotsConfig ignores peers until enabled

	pubOfferUID string // UID of the last publisherSdpOffer sent

	closeCh chan struct{}
	once    sync.Once
}

type iceCandidate struct {
	Candidate string `json:"candidate"`
	SDPMid    string `json:"sdpMid"`
	Target    string `json:"target"` // "PUBLISHER" or "SUBSCRIBER"
}

// ConnectGoloom connects to the Goloom WebSocket, sends the hello message,
// and returns the client after receiving the serverHello with TURN credentials.
func ConnectGoloom(ctx context.Context, conf *conferenceInfo, logger *slog.Logger) (*GoloomClient, *serverHello, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: httpTimeout,
	}
	headers := http.Header{
		"Origin":     {"https://telemost.yandex.com"},
		"User-Agent": {userAgent},
	}

	conn, _, err := dialer.DialContext(ctx, conf.MediaServerURL, headers)
	if err != nil {
		return nil, nil, fmt.Errorf("ws connect: %w", err)
	}

	gc := &GoloomClient{
		conn:        conn,
		logger:      logger,
		pubAnswer:   make(chan string, 1),
		pubError:    make(chan string, 1),
		subOffer:    make(chan string, 4),
		serverICE:   make(chan iceCandidate, 32),
		peerUpdates:   make(chan []Participant, 8),
		upsertUpdates: make(chan []Participant, 8),
		newPeerCh:     make(chan string, 16),
		closeCh:     make(chan struct{}),
	}

	// Send hello.
	hello := buildHello(conf)
	if err := gc.writeJSON(hello); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("send hello: %w", err)
	}

	// Read until serverHello.
	sh, err := gc.readServerHello()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("read serverHello: %w", err)
	}
	gc.serverHelloData = sh

	// Start background reader and ping loop.
	go gc.readPump()
	go gc.pingLoop(ctx)

	return gc, sh, nil
}

// ICEServers returns the WebRTC ICE server configuration from the serverHello.
func (gc *GoloomClient) ICEServers() []webrtc.ICEServer {
	if gc.serverHelloData == nil {
		return nil
	}
	var servers []webrtc.ICEServer
	for _, srv := range gc.serverHelloData.RtcConfiguration.IceServers {
		s := webrtc.ICEServer{URLs: []string(srv.URLs)}
		if srv.Username != "" {
			s.Username = srv.Username
			s.Credential = srv.Credential
			s.CredentialType = webrtc.ICECredentialTypePassword
		}
		servers = append(servers, s)
	}
	return servers
}

// SendPublisherOffer sends the publisher SDP offer and waits for the answer.
func (gc *GoloomClient) SendPublisherOffer(ctx context.Context, sdp string) (string, error) {
	uid := uuid.New().String()
	msg := map[string]interface{}{
		"uid": uid,
		"publisherSdpOffer": map[string]interface{}{
			"pcSeq":  1,
			"sdp":    sdp,
			"tracks": []interface{}{},
		},
	}
	gc.logger.Debug("sending publisherSdpOffer", "uid", uid)
	gc.mu.Lock()
	gc.pubOfferUID = uid
	gc.mu.Unlock()

	if err := gc.writeJSON(msg); err != nil {
		return "", fmt.Errorf("send publisher offer: %w", err)
	}

	select {
	case answer := <-gc.pubAnswer:
		return answer, nil
	case errMsg := <-gc.pubError:
		return "", fmt.Errorf("publisher offer rejected: %s", errMsg)
	case <-ctx.Done():
		return "", ctx.Err()
	case <-gc.closeCh:
		return "", fmt.Errorf("goloom connection closed")
	}
}

// WaitSubscriberOffer waits for a subscriberSdpOffer from the server.
func (gc *GoloomClient) WaitSubscriberOffer(ctx context.Context) (string, error) {
	select {
	case offer := <-gc.subOffer:
		return offer, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-gc.closeCh:
		return "", fmt.Errorf("goloom connection closed")
	}
}

// WaitSubscriberOfferCh returns the channel for receiving subsequent subscriber SDP offers
// (used for re-negotiation after the initial offer).
func (gc *GoloomClient) WaitSubscriberOfferCh() <-chan string {
	return gc.subOffer
}

// SendSubscriberAnswer sends the subscriber SDP answer.
func (gc *GoloomClient) SendSubscriberAnswer(sdp string) error {
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"subscriberSdpAnswer": map[string]interface{}{
			"pcSeq": 1,
			"sdp":   sdp,
		},
	}
	return gc.writeJSON(msg)
}

// SendUpdateMe tells the SFU that we're sending audio/video.
// This must be sent after publisher SDP exchange to activate media forwarding.
func (gc *GoloomClient) SendUpdateMe(name string, sendAudio, sendVideo bool) error {
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"updateMe": map[string]interface{}{
			"participantMeta": map[string]interface{}{
				"name":        name,
				"description": "",
				"role":        "SPEAKER",
				"sendAudio":   sendAudio,
				"sendVideo":   sendVideo,
			},
			"participantAttributes": map[string]interface{}{
				"name":        name,
				"role":        "SPEAKER",
				"description": "",
			},
			"sendAudio":   sendAudio,
			"sendVideo":   sendVideo,
			"sendSharing": false,
		},
	}
	return gc.writeJSON(msg)
}

// SendPublisherTrackDescription advertises our publisher tracks to the SFU.
// This must be sent together with updateMe to activate video forwarding.
func (gc *GoloomClient) SendPublisherTrackDescription(sendAudio, sendVideo bool) error {
	tracks := []map[string]interface{}{}
	if sendAudio {
		tracks = append(tracks, map[string]interface{}{
			"mid":            "0",
			"transceiverMid": "0",
			"kind":           "AUDIO",
			"priority":       0,
			"label":          "microphone",
			"codecs":         map[string]interface{}{},
			"groupId":        1,
			"description":    "",
		})
	}
	if sendVideo {
		tracks = append(tracks, map[string]interface{}{
			"mid":            "1",
			"transceiverMid": "1",
			"kind":           "VIDEO",
			"priority":       0,
			"label":          "camera",
			"codecs":         map[string]interface{}{},
			"groupId":        1,
			"description":    "",
		})
	}
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"updatePublisherTrackDescription": map[string]interface{}{
			"publisherTrackDescriptions": tracks,
		},
	}
	return gc.writeJSON(msg)
}

// SendSetSlots tells the SFU what video slots we want to receive.
func (gc *GoloomClient) SendSetSlots() error {
	// Use enough slots to cover ghost participants from previous sessions
	// plus live participants. SFU only forwards video for active slots.
	slots := make([]map[string]interface{}, 8)
	for i := range slots {
		slots[i] = map[string]interface{}{
			"width":  640,
			"height": 480,
		}
	}
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"setSlots": map[string]interface{}{
			"slots":              slots,
			"audioSlotsCount":    1,
			"withSelfView":       false,
			"selfViewVisibility": "HIDDEN",
		},
	}
	return gc.writeJSON(msg)
}

// SendICECandidate sends an ICE candidate to the server.
func (gc *GoloomClient) SendICECandidate(candidate, sdpMid, target string) error {
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"webrtcIceCandidate": map[string]interface{}{
			"pcSeq":     1,
			"candidate": candidate,
			"sdpMid":    sdpMid,
			"target":    target,
		},
	}
	return gc.writeJSON(msg)
}

// RecvICECandidate returns the channel for receiving server ICE candidates.
func (gc *GoloomClient) RecvICECandidate() <-chan iceCandidate {
	return gc.serverICE
}

// parseUpdateDescription extracts participant info from an updateDescription message.
func (gc *GoloomClient) parseUpdateDescription(raw json.RawMessage) {
	var desc struct {
		Description []struct {
			ID   string `json:"id"`
			Meta struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				SendVideo   bool   `json:"sendVideo"`
			} `json:"meta"`
			DisconnectedAt int64 `json:"disconnectedAt,omitempty"`
		} `json:"description"`
	}
	if err := json.Unmarshal(raw, &desc); err != nil {
		return
	}
	peers := make([]Participant, len(desc.Description))
	for i, d := range desc.Description {
		peers[i] = Participant{
			ID:             d.ID,
			Name:           d.Meta.Name,
			Description:    d.Meta.Description,
			SendVideo:      d.Meta.SendVideo,
			DisconnectedAt: d.DisconnectedAt,
		}
		// Mark all participants from updateDescription as known
		// (self, ghosts, other server peers) for slotsConfig-based discovery.
		gc.knownPeers.Store(d.ID, true)
	}
	select {
	case gc.peerUpdates <- peers:
	default:
		// Drop oldest, push new.
		select {
		case <-gc.peerUpdates:
		default:
		}
		gc.peerUpdates <- peers
	}
}

// parseUpsertDescription pushes incremental participant updates to upsertUpdates channel.
// Only called for upsertDescription messages (not initial updateDescription).
func (gc *GoloomClient) parseUpsertDescription(raw json.RawMessage) {
	var desc struct {
		Description []struct {
			ID   string `json:"id"`
			Meta struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				SendVideo   bool   `json:"sendVideo"`
			} `json:"meta"`
			DisconnectedAt int64 `json:"disconnectedAt,omitempty"`
		} `json:"description"`
	}
	if err := json.Unmarshal(raw, &desc); err != nil {
		return
	}
	peers := make([]Participant, len(desc.Description))
	for i, d := range desc.Description {
		peers[i] = Participant{
			ID:             d.ID,
			Name:           d.Meta.Name,
			Description:    d.Meta.Description,
			SendVideo:      d.Meta.SendVideo,
			DisconnectedAt: d.DisconnectedAt,
		}
	}
	select {
	case gc.upsertUpdates <- peers:
	default:
		select {
		case <-gc.upsertUpdates:
		default:
		}
		gc.upsertUpdates <- peers
	}
}

// WaitForNewPeerByName blocks until a NEWLY joined participant with the given
// name appears via upsertDescription. Unlike WaitForPeer, this ignores
// participants from the initial updateDescription (existing/ghost participants).
func (gc *GoloomClient) WaitForNewPeerByName(ctx context.Context, peerName string) (string, error) {
	for {
		select {
		case peers := <-gc.upsertUpdates:
			for _, p := range peers {
				if p.Name == peerName && p.IsLive() {
					return p.ID, nil
				}
			}
		case <-ctx.Done():
			return "", ctx.Err()
		case <-gc.closeCh:
			return "", fmt.Errorf("goloom closed")
		}
	}
}

// parseParticipantConnected handles a participantConnected event from the SFU.
// This fires when a new participant joins the conference and is the primary
// way existing participants learn about new joiners (updateDescription is only
// sent once, at initial connection).
func (gc *GoloomClient) parseParticipantConnected(raw json.RawMessage) {
	var pc struct {
		ID   string `json:"id"`
		Meta struct {
			Name      string `json:"name"`
			SendVideo bool   `json:"sendVideo"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &pc); err != nil {
		return
	}
	if pc.ID == "" {
		return
	}
	peers := []Participant{{
		ID:        pc.ID,
		Name:      pc.Meta.Name,
		SendVideo: pc.Meta.SendVideo,
	}}
	select {
	case gc.peerUpdates <- peers:
	default:
		select {
		case <-gc.peerUpdates:
		default:
		}
		gc.peerUpdates <- peers
	}
}

// AddKnownPeer marks a participant ID as known (self, ghost, or already-paired).
// Known peers are excluded from WaitForNewPeer results.
func (gc *GoloomClient) AddKnownPeer(id string) {
	gc.knownPeers.Store(id, true)
}

// EnableDiscovery enables slotsConfig-based peer discovery.
// Must be called after all known peers (own server participants) are registered.
func (gc *GoloomClient) EnableDiscovery() {
	gc.discoveryEnabled.Store(true)
}

// parseSlotsConfig extracts participant IDs from a slotsConfig message
// and pushes newly discovered (unknown) IDs to the newPeerCh channel.
func (gc *GoloomClient) parseSlotsConfig(raw json.RawMessage) {
	if !gc.discoveryEnabled.Load() {
		return
	}
	var sc struct {
		Slots []struct {
			Participant          *struct{ ParticipantId string `json:"participantId"` } `json:"participant,omitempty"`
			ParticipantVideoByMid *struct{ ParticipantId string `json:"participantId"` } `json:"participantVideoByMid,omitempty"`
		} `json:"slots"`
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		return
	}
	for _, slot := range sc.Slots {
		var pid string
		if slot.ParticipantVideoByMid != nil {
			pid = slot.ParticipantVideoByMid.ParticipantId
		} else if slot.Participant != nil {
			pid = slot.Participant.ParticipantId
		}
		if pid == "" {
			continue
		}
		if _, known := gc.knownPeers.Load(pid); known {
			continue
		}
		gc.knownPeers.Store(pid, true)
		select {
		case gc.newPeerCh <- pid:
		default:
		}
	}
}

// WaitForNewPeer blocks until a previously-unknown participant ID appears
// in slotsConfig. Used by the server which can't see participant names
// (updateDescription is only sent at join time, not to existing participants).
func (gc *GoloomClient) WaitForNewPeer(ctx context.Context) (string, error) {
	select {
	case id := <-gc.newPeerCh:
		return id, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-gc.closeCh:
		return "", fmt.Errorf("goloom closed")
	}
}

// WaitForPeer blocks until a live participant with the given name appears
// in updateDescription. Returns the participant ID.
func (gc *GoloomClient) WaitForPeer(ctx context.Context, peerName string) (string, error) {
	for {
		select {
		case peers := <-gc.peerUpdates:
			for _, p := range peers {
				if p.Name == peerName && p.IsLive() {
					return p.ID, nil
				}
			}
		case <-ctx.Done():
			return "", ctx.Err()
		case <-gc.closeCh:
			return "", fmt.Errorf("goloom closed")
		}
	}
}

// FindAllPeersByName collects all live participants matching peerName from
// the initial updateDescription batch. If a match appears in upsertUpdates
// (fresh joiner, definitely not a ghost), it is returned immediately as the
// sole candidate. Returns at least one candidate or an error.
func (gc *GoloomClient) FindAllPeersByName(ctx context.Context, peerName string) ([]Participant, error) {
	for {
		select {
		case peers := <-gc.peerUpdates:
			var matches []Participant
			for _, p := range peers {
				if p.Name == peerName && p.IsLive() {
					matches = append(matches, p)
				}
			}
			if len(matches) > 0 {
				return matches, nil
			}
		case peers := <-gc.upsertUpdates:
			// Fresh joiner — definitely not a ghost.
			for _, p := range peers {
				if p.Name == peerName && p.IsLive() {
					return []Participant{p}, nil
				}
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-gc.closeCh:
			return nil, fmt.Errorf("goloom closed")
		}
	}
}

// SendVPNReady updates our participant description to "vpn-ready:<nonce>" via updateMe.
// The nonce is a hex-encoded random value used to derive a session-specific obfuscation
// key, preventing ghost participants from previous sessions with the same token from
// interfering with SSRC locking. Returns the nonce.
func (gc *GoloomClient) SendVPNReady(name string, nonce string) error {
	desc := "vpn-ready:" + nonce
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"updateMe": map[string]interface{}{
			"participantMeta": map[string]interface{}{
				"name":        name,
				"description": desc,
				"role":        "SPEAKER",
				"sendAudio":   true,
				"sendVideo":   true,
			},
			"participantAttributes": map[string]interface{}{
				"name":        name,
				"role":        "SPEAKER",
				"description": desc,
			},
			"sendAudio":   true,
			"sendVideo":   true,
			"sendSharing": false,
		},
	}
	return gc.writeJSON(msg)
}

// WaitPeerReady blocks until a participant with the given name has description
// starting with "vpn-ready:" in an upsertDescription update. Returns the nonce.
func (gc *GoloomClient) WaitPeerReady(ctx context.Context, peerName string) (string, error) {
	const prefix = "vpn-ready:"
	for {
		select {
		case peers := <-gc.upsertUpdates:
			for _, p := range peers {
				if p.Name == peerName && strings.HasPrefix(p.Description, prefix) && p.IsLive() {
					return strings.TrimPrefix(p.Description, prefix), nil
				}
			}
		case <-ctx.Done():
			return "", ctx.Err()
		case <-gc.closeCh:
			return "", fmt.Errorf("goloom closed")
		}
	}
}

// PinToPeer sends a setSlots request that pins to a specific participant,
// ensuring the SFU only forwards that participant's video to us.
// Uses 8 slots (matching initial SendSetSlots), with the first slot pinned
// to the target participant and remaining slots empty for SFU auto-fill.
func (gc *GoloomClient) PinToPeer(participantID string) error {
	slots := make([]map[string]interface{}, 8)
	slots[0] = map[string]interface{}{
		"width":  640,
		"height": 480,
		"participant": map[string]interface{}{
			"participantId": participantID,
		},
		"pinned": true,
		"label":  "camera",
	}
	for i := 1; i < len(slots); i++ {
		slots[i] = map[string]interface{}{
			"width":  640,
			"height": 480,
		}
	}
	msg := map[string]interface{}{
		"uid": uuid.New().String(),
		"setSlots": map[string]interface{}{
			"slots":              slots,
			"audioSlotsCount":    1,
			"withSelfView":       false,
			"selfViewVisibility": "HIDDEN",
		},
	}
	gc.logger.Info("pinning to peer", "participant_id", participantID)
	return gc.writeJSON(msg)
}

// Close sends a leave message and closes the Goloom WebSocket connection.
// The leave message tells the SFU to immediately remove this participant,
// preventing ghost participants from accumulating in the conference.
func (gc *GoloomClient) Close() error {
	gc.once.Do(func() {
		close(gc.closeCh)
		// Send leave before closing so the SFU removes participant immediately.
		_ = gc.writeJSON(map[string]interface{}{
			"uid":   uuid.New().String(),
			"leave": map[string]interface{}{},
		})
		gc.conn.Close()
	})
	return nil
}

// --- internal ---

func (gc *GoloomClient) writeJSON(v interface{}) error {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.conn.WriteJSON(v)
}

func (gc *GoloomClient) sendAck(uid string) {
	ack := map[string]interface{}{
		"uid": uid,
		"ack": map[string]interface{}{
			"status": map[string]string{
				"code":        "OK",
				"description": "",
			},
		},
	}
	_ = gc.writeJSON(ack)
}

// readServerHello reads messages until serverHello is found.
func (gc *GoloomClient) readServerHello() (*serverHello, error) {
	gc.conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	defer gc.conn.SetReadDeadline(time.Time{})

	for {
		_, msg, err := gc.conn.ReadMessage()
		if err != nil {
			return nil, err
		}

		var env struct {
			UID         string           `json:"uid"`
			Ack         *json.RawMessage `json:"ack,omitempty"`
			ServerHello *serverHello     `json:"serverHello,omitempty"`
		}
		if err := json.Unmarshal(msg, &env); err != nil {
			continue
		}

		if env.Ack == nil && env.UID != "" {
			gc.sendAck(env.UID)
		}

		if env.ServerHello != nil {
			return env.ServerHello, nil
		}
	}
}

// readPump is the background reader goroutine.
func (gc *GoloomClient) readPump() {
	for {
		select {
		case <-gc.closeCh:
			return
		default:
		}

		_, msg, err := gc.conn.ReadMessage()
		if err != nil {
			gc.logger.Debug("goloom read error", "err", err)
			gc.Close()
			return
		}

		gc.handleMessage(msg)
	}
}

func (gc *GoloomClient) handleMessage(msg []byte) {
	gc.logger.Debug("goloom recv", "len", len(msg))

	var env struct {
		UID                  string           `json:"uid"`
		Ack                  *json.RawMessage `json:"ack,omitempty"`
		PublisherSdpAnswer   *sdpPayload      `json:"publisherSdpAnswer,omitempty"`
		SubscriberSdpOffer   *sdpPayload      `json:"subscriberSdpOffer,omitempty"`
		WebrtcIceCandidate   *iceCandidate    `json:"webrtcIceCandidate,omitempty"`
		UpdateDescription    *json.RawMessage `json:"updateDescription,omitempty"`
		UpsertDescription    *json.RawMessage `json:"upsertDescription,omitempty"`
		VadActivity          *json.RawMessage `json:"vadActivity,omitempty"`
		SlotsConfig          *json.RawMessage `json:"slotsConfig,omitempty"`
		ParticipantConnected *json.RawMessage `json:"participantConnected,omitempty"`
	}
	if err := json.Unmarshal(msg, &env); err != nil {
		return
	}

	// Handle ack errors for our publisher offer.
	if env.Ack != nil && env.UID != "" {
		var ackData struct {
			Status struct {
				Code        string `json:"code"`
				Description string `json:"description"`
			} `json:"status"`
		}
		json.Unmarshal(*env.Ack, &ackData)

		gc.mu.Lock()
		isPubOffer := env.UID == gc.pubOfferUID
		gc.mu.Unlock()

		if isPubOffer && ackData.Status.Code != "OK" && ackData.Status.Code != "" {
			select {
			case gc.pubError <- ackData.Status.Code:
			default:
			}
		}
		return // acks don't need further processing
	}

	// Send ack for non-ack messages.
	if env.UID != "" {
		gc.sendAck(env.UID)
	}

	if env.PublisherSdpAnswer != nil {
		select {
		case gc.pubAnswer <- env.PublisherSdpAnswer.SDP:
		default:
		}
	}

	if env.SubscriberSdpOffer != nil {
		select {
		case gc.subOffer <- env.SubscriberSdpOffer.SDP:
		default:
			gc.logger.Warn("subscriber offer dropped (buffer full)")
		}
	}

	if env.WebrtcIceCandidate != nil {
		select {
		case gc.serverICE <- *env.WebrtcIceCandidate:
		default:
		}
	}

	if env.SlotsConfig != nil {
		gc.logger.Info("slotsConfig", "data", string(*env.SlotsConfig))
		gc.parseSlotsConfig(*env.SlotsConfig)
	}
	if env.UpdateDescription != nil {
		gc.logger.Info("updateDescription", "data", string(*env.UpdateDescription))
		gc.parseUpdateDescription(*env.UpdateDescription)
	}
	if env.UpsertDescription != nil {
		gc.logger.Info("upsertDescription", "data", string(*env.UpsertDescription))
		gc.parseUpdateDescription(*env.UpsertDescription)
		gc.parseUpsertDescription(*env.UpsertDescription)
	}
	if env.ParticipantConnected != nil {
		gc.logger.Info("participantConnected", "data", string(*env.ParticipantConnected))
		gc.parseParticipantConnected(*env.ParticipantConnected)
	}
}

func (gc *GoloomClient) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-gc.closeCh:
			return
		case <-ticker.C:
			msg := map[string]interface{}{
				"uid":  uuid.New().String(),
				"ping": map[string]interface{}{},
			}
			if err := gc.writeJSON(msg); err != nil {
				gc.logger.Debug("ping failed", "err", err)
				return
			}
		}
	}
}

type sdpPayload struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}
