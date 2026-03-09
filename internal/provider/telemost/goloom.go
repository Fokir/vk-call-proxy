package telemost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

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
		conn:      conn,
		logger:    logger,
		pubAnswer: make(chan string, 1),
		pubError:  make(chan string, 1),
		subOffer:  make(chan string, 4),
		serverICE: make(chan iceCandidate, 32),
		closeCh:   make(chan struct{}),
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
	// Request enough slots to cover all participants (including stale ones).
	slots := make([]map[string]interface{}, 12)
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
			"audioSlotsCount":    8,
			"withSelfView":      false,
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

// Close closes the Goloom WebSocket connection.
func (gc *GoloomClient) Close() error {
	gc.once.Do(func() {
		close(gc.closeCh)
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
	}
	if env.UpdateDescription != nil {
		gc.logger.Info("updateDescription", "data", string(*env.UpdateDescription))
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
