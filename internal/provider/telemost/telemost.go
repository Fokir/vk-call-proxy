// Package telemost implements provider.Service for Yandex Telemost.
// It tunnels VPN data through the Goloom SFU via WebRTC DataChannels.
//
// Both VPN client and server join the same Telemost meeting, establish
// WebRTC PeerConnections with the SFU, and exchange data through DataChannels
// that the SFU forwards between participants.
package telemost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

// sortedPairID creates a deterministic nonce from two participant IDs
// by sorting them lexicographically and concatenating with a separator.
func sortedPairID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + ":" + b
}

const (
	httpTimeout   = 20 * time.Second
	wsReadTimeout = 15 * time.Second
	pingInterval  = 5 * time.Second
	userAgent     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:144.0) Gecko/20100101 Firefox/144.0"
	clientVersion = "183.3.0"
	sdkVersion    = "5.24.1"
	conferenceAPI = "https://cloud-api.yandex.com/telemost_front/v2/telemost/conferences"
)

// Service implements provider.Service for Yandex Telemost.
type Service struct {
	meetingID string
	authToken string   // stored for per-connection key derivation
	obfKey    [32]byte // XOR obfuscation key for VP8 payload masking (default, index=0)
}

// Compile-time check.
var _ provider.Service = (*Service)(nil)

// NewService creates a Telemost service provider.
// meetingURL can be a full URL (https://telemost.yandex.com/j/12345) or just the meeting ID.
// authToken is used to derive the obfuscation key for VP8 payload masking.
func NewService(meetingURL string, authToken string) *Service {
	id := extractMeetingID(meetingURL)
	return &Service{
		meetingID: id,
		authToken: authToken,
		obfKey:    DeriveObfuscationKey(authToken),
	}
}

func (s *Service) Name() string { return "telemost" }

// FetchCredentials obtains TURN credentials from Telemost by joining
// a meeting as a guest, connecting to the Goloom media server,
// and extracting ICE server credentials from the serverHello response.
func (s *Service) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	ji, err := s.FetchJoinInfo(ctx)
	if err != nil {
		return nil, err
	}
	creds := ji.Credentials
	return &creds, nil
}

// FetchJoinInfo performs the full Telemost join flow:
// 1. GET conference connection info (room_id, peer_id, credentials, media_server_url)
// 2. Connect to Goloom WebSocket and send hello
// 3. Receive serverHello with TURN credentials
func (s *Service) FetchJoinInfo(ctx context.Context) (*provider.JoinInfo, error) {
	client := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	// Step 1: Get conference connection info.
	conf, err := getConferenceConnection(ctx, client, s.meetingID)
	if err != nil {
		return nil, fmt.Errorf("get conference: %w", err)
	}

	// Step 2: Connect to Goloom and extract TURN credentials.
	creds, err := goloomHandshake(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("goloom handshake: %w", err)
	}

	return &provider.JoinInfo{
		Credentials: *creds,
		WSEndpoint:  conf.MediaServerURL,
		ConvID:      conf.RoomID,
	}, nil
}

// ConnectSignaling is not supported for Telemost (SFU model, no peer signaling).
func (s *Service) ConnectSignaling(_ context.Context, _ *provider.JoinInfo, _ *slog.Logger) (provider.SignalingClient, error) {
	return nil, fmt.Errorf("telemost: use Connect() for WebRTC DataChannel transport")
}

// PendingConn represents a Telemost WebRTC connection that has been set up
// but is not yet ready (waiting for the other peer's video track).
type PendingConn struct {
	transport *WebRTCTransport
	goloom    *GoloomClient
	peerID    string // our own participant ID in the conference
	myName    string // our display name for signaling
	authToken string // for session-specific obfKey derivation
	index     int    // connection index for obfKey derivation
}

// WaitReady blocks until the subscriber receives a video track from
// the other participant. Returns the bidirectional RTPConn.
func (pc *PendingConn) WaitReady(ctx context.Context) (io.ReadWriteCloser, func(), error) {
	if err := pc.transport.WaitReady(ctx); err != nil {
		pc.Close()
		return nil, nil, err
	}
	cleanup := func() {
		pc.transport.Close()
		pc.goloom.Close()
	}
	return pc.transport.RTPConn(), cleanup, nil
}

// Close tears down the pending connection without waiting.
func (pc *PendingConn) Close() {
	pc.transport.Close()
	pc.goloom.Close()
}

// OwnPeerID returns our participant ID in the conference.
func (pc *PendingConn) OwnPeerID() string {
	return pc.peerID
}

// AddKnownPeer marks a participant ID as known (excluded from WaitForNewPeer).
func (pc *PendingConn) AddKnownPeer(id string) {
	pc.goloom.AddKnownPeer(id)
}

// EnableDiscovery enables slotsConfig-based peer discovery on this connection.
func (pc *PendingConn) EnableDiscovery() {
	pc.goloom.EnableDiscovery()
}

// WaitPinReady waits for a peer with the given name to appear via
// upsertDescription, pins to it, then waits for the subscriber track.
// Pinning must happen BEFORE waiting for the track because when ghost
// participants fill all SFU slots, the subscriber gets no video until
// we explicitly pin to an active publisher.
func (pc *PendingConn) WaitPinReady(ctx context.Context, logger *slog.Logger, peerName string) (io.ReadWriteCloser, func(), error) {
	// Wait for the peer to join (via upsertDescription only — ignores pre-existing participants).
	peerID, err := pc.goloom.WaitForNewPeerByName(ctx, peerName)
	if err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("wait for peer %q: %w", peerName, err)
	}
	logger.Info("peer found by name, pinning", "peer_name", peerName, "peer_id", peerID)

	// Pin first — this makes the SFU assign the active peer to our subscriber slot.
	if err := pc.goloom.PinToPeer(peerID); err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("pin to peer: %w", err)
	}
	pc.transport.ResetForPin()

	// Now wait for subscriber track — should fire quickly after pin to active publisher.
	if err := pc.transport.WaitReady(ctx); err != nil {
		pc.Close()
		return nil, nil, err
	}

	// Derive session-specific obfuscation key from both participant IDs.
	sessionKey := DeriveSessionObfuscationKey(pc.authToken, pc.index, sortedPairID(pc.peerID, peerID))
	pc.transport.RTPConn().SetObfKey(sessionKey)
	logger.Info("session obfKey set", "my_id", pc.peerID[:8], "peer_id", peerID[:8])

	cleanup := func() {
		pc.transport.Close()
		pc.goloom.Close()
	}
	return pc.transport.RTPConn(), cleanup, nil
}

// Setup joins a Telemost meeting and sets up WebRTC PeerConnections
// without blocking on the other peer. Returns a PendingConn that must
// be completed with WaitReady once the other peer has joined.
func (s *Service) Setup(ctx context.Context, logger *slog.Logger) (*PendingConn, error) {
	return s.SetupNamed(ctx, logger, "vpn-peer")
}

// SetupNamed is like Setup but uses a custom participant name.
// Uses the default obfuscation key (index=0).
func (s *Service) SetupNamed(ctx context.Context, logger *slog.Logger, name string) (*PendingConn, error) {
	return s.SetupNamedIndexed(ctx, logger, name, 0)
}

// SetupNamedIndexed is like SetupNamed but uses a per-connection obfuscation key
// derived from the auth token and connection index. This ensures that each
// connection pair (server[i], client[i]) uses a unique key, preventing
// cross-talk between connections sharing the same Telemost conference.
func (s *Service) SetupNamedIndexed(ctx context.Context, logger *slog.Logger, name string, index int) (*PendingConn, error) {
	client := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	conf, err := getConferenceConnection(ctx, client, s.meetingID)
	if err != nil {
		return nil, fmt.Errorf("get conference: %w", err)
	}
	logger.Info("joined Telemost conference",
		"room_id", conf.RoomID,
		"peer_id", conf.PeerID,
		"media_server", conf.MediaServerURL,
	)

	goloom, _, err := ConnectGoloom(ctx, conf, logger.With("component", "goloom"))
	if err != nil {
		return nil, fmt.Errorf("goloom connect: %w", err)
	}

	obfKey := DeriveIndexedObfuscationKey(s.authToken, index)
	transport, err := SetupWebRTC(ctx, goloom, logger.With("component", "webrtc"), obfKey, name)
	if err != nil {
		goloom.Close()
		return nil, fmt.Errorf("setup webrtc: %w", err)
	}

	return &PendingConn{transport: transport, goloom: goloom, peerID: conf.PeerID, myName: name, authToken: s.authToken, index: index}, nil
}

// Connect joins a Telemost meeting and establishes a WebRTC connection
// through the Goloom SFU. Blocks until the other peer joins and video
// tracks are active. Returns a bidirectional io.ReadWriteCloser for MUX.
func (s *Service) Connect(ctx context.Context, logger *slog.Logger) (io.ReadWriteCloser, func(), error) {
	pc, err := s.Setup(ctx, logger)
	if err != nil {
		return nil, nil, err
	}
	return pc.WaitReady(ctx)
}

// ConnectPaired joins a Telemost meeting with a named participant,
// waits for a specific peer by name, pins video to that peer,
// then waits for the connection to be ready.
// index is used to derive a per-connection obfuscation key.
//
// When ghost participants from previous sessions are in the conference with
// the same display name, ConnectPaired probes each candidate with a short
// VP8 data validity check (correct obfuscation key → valid magic).
func (s *Service) ConnectPaired(ctx context.Context, logger *slog.Logger, myName, peerName string, index int) (io.ReadWriteCloser, func(), error) {
	pc, err := s.SetupNamedIndexed(ctx, logger, myName, index)
	if err != nil {
		return nil, nil, err
	}

	// Collect all candidates matching the peer name (may include ghosts).
	// NOTE: Do NOT call WaitReady here — the SFU may assign ghost slots initially,
	// and the subscriber video track only fires after pinning to an active publisher.
	candidates, err := pc.goloom.FindAllPeersByName(ctx, peerName)
	if err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("find peer %q: %w", peerName, err)
	}
	logger.Info("peer candidates found", "name", peerName, "count", len(candidates))

	// Single candidate — pin and wait for subscriber track (old behavior).
	if len(candidates) == 1 {
		return s.pinAndFinish(ctx, pc, candidates[0].ID, logger)
	}

	// Multiple candidates — probe each to find the one with matching obfKey.
	const probeTimeout = 5 * time.Second
	for i, cand := range candidates {
		logger.Info("probing candidate", "i", i, "id", cand.ID[:8], "name", cand.Name)

		if err := pc.goloom.PinToPeer(cand.ID); err != nil {
			logger.Warn("pin failed, skipping", "id", cand.ID[:8], "err", err)
			continue
		}
		pc.transport.ResetForPin()

		sessionKey := DeriveSessionObfuscationKey(pc.authToken, pc.index, sortedPairID(pc.peerID, cand.ID))
		pc.transport.RTPConn().SetObfKey(sessionKey)

		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := pc.transport.RTPConn().WaitValidData(probeCtx)
		cancel()

		if err == nil {
			logger.Info("valid data from candidate", "id", cand.ID[:8])
			cleanup := func() {
				pc.transport.Close()
				pc.goloom.Close()
			}
			return pc.transport.RTPConn(), cleanup, nil
		}
		logger.Info("candidate rejected (no valid data)", "id", cand.ID[:8], "err", err)
	}

	pc.Close()
	return nil, nil, fmt.Errorf("no valid peer found among %d candidates for %q", len(candidates), peerName)
}

// pinAndFinish pins to a single peer candidate, waits for subscriber track, and returns.
func (s *Service) pinAndFinish(ctx context.Context, pc *PendingConn, peerID string, logger *slog.Logger) (io.ReadWriteCloser, func(), error) {
	logger.Info("peer found, pinning", "peer_id", peerID)

	if err := pc.goloom.PinToPeer(peerID); err != nil {
		pc.Close()
		return nil, nil, fmt.Errorf("pin to peer: %w", err)
	}
	pc.transport.ResetForPin()

	sessionKey := DeriveSessionObfuscationKey(pc.authToken, pc.index, sortedPairID(pc.peerID, peerID))
	pc.transport.RTPConn().SetObfKey(sessionKey)
	logger.Info("session obfKey set", "my_id", pc.peerID[:8], "peer_id", peerID[:8])

	return pc.WaitReady(ctx)
}

// HTTPClient returns a configured HTTP client for Telemost API.
func (s *Service) HTTPClient() *http.Client {
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
}

// GetConference gets conference connection info for testing/advanced use.
func (s *Service) GetConference(ctx context.Context, client *http.Client) (*ConferenceInfo, error) {
	return getConferenceConnection(ctx, client, s.meetingID)
}

// --- conference connection ---

// ConferenceInfo holds conference connection details.
type ConferenceInfo = conferenceInfo

type conferenceInfo struct {
	RoomID         string
	PeerID         string
	Credentials    string
	MediaServerURL string
}

func getConferenceConnection(ctx context.Context, client *http.Client, meetingID string) (*conferenceInfo, error) {
	meetingURI := fmt.Sprintf("https://telemost.yandex.ru/j/%s", meetingID)
	encodedURI := url.PathEscape(meetingURI)
	displayName := url.QueryEscape(provider.RandomDisplayName())

	endpoint := fmt.Sprintf("%s/%s/connection?next_gen_media_platform_allowed=true&display_name=%s&waiting_room_supported=true",
		conferenceAPI, encodedURI, displayName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://telemost.yandex.com/")
	req.Header.Set("Origin", "https://telemost.yandex.com")
	req.Header.Set("Client-Instance-Id", uuid.New().String())
	req.Header.Set("X-Telemost-Client-Version", clientVersion)
	req.Header.Set("Idempotency-Key", uuid.New().String())

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		RoomID              string `json:"room_id"`
		PeerID              string `json:"peer_id"`
		Credentials         string `json:"credentials"`
		ClientConfiguration struct {
			MediaServerURL string `json:"media_server_url"`
		} `json:"client_configuration"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if data.RoomID == "" || data.PeerID == "" {
		return nil, fmt.Errorf("missing room_id or peer_id: %s", string(body))
	}
	if data.ClientConfiguration.MediaServerURL == "" {
		return nil, fmt.Errorf("missing media_server_url: %s", string(body))
	}

	return &conferenceInfo{
		RoomID:         data.RoomID,
		PeerID:         data.PeerID,
		Credentials:    data.Credentials,
		MediaServerURL: data.ClientConfiguration.MediaServerURL,
	}, nil
}

// goloomHandshake connects to Goloom, sends hello, and extracts TURN credentials.
// Used by FetchCredentials/FetchJoinInfo for simple credential fetching.
func goloomHandshake(ctx context.Context, conf *conferenceInfo) (*provider.Credentials, error) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gc, sh, err := ConnectGoloom(ctx, conf, logger)
	if err != nil {
		return nil, err
	}
	gc.Close()
	return extractTURNCreds(sh)
}

type serverHello struct {
	RtcConfiguration struct {
		IceServers []iceServer `json:"iceServers"`
	} `json:"rtcConfiguration"`
}

type iceServer struct {
	URLs       flexURLs `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// flexURLs handles both string and []string JSON for the "urls" field.
type flexURLs []string

func (f *flexURLs) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*f = []string{single}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(data, &arr); err != nil {
		return err
	}
	*f = arr
	return nil
}

func extractTURNCreds(sh *serverHello) (*provider.Credentials, error) {
	for _, srv := range sh.RtcConfiguration.IceServers {
		if srv.Username == "" || srv.Credential == "" {
			continue // skip STUN-only entries
		}
		for _, u := range srv.URLs {
			if !strings.HasPrefix(u, "turn:") && !strings.HasPrefix(u, "turns:") {
				continue
			}
			host, port := turn.ParseTURNURL(u)
			return &provider.Credentials{
				Username: srv.Username,
				Password: srv.Credential,
				Host:     host,
				Port:     port,
			}, nil
		}
	}
	return nil, fmt.Errorf("no TURN credentials in serverHello")
}

// --- hello message builder ---

func buildHello(conf *conferenceInfo) map[string]interface{} {
	return map[string]interface{}{
		"uid": uuid.New().String(),
		"hello": map[string]interface{}{
			"participantMeta": map[string]interface{}{
				"name":      provider.RandomDisplayName(),
				"role":      "SPEAKER",
				"sendAudio": true,
				"sendVideo": true,
			},
			"participantAttributes": map[string]interface{}{},
			"roomId":                conf.RoomID,
			"participantId":         conf.PeerID,
			"serviceName":           "telemost",
			"credentials":           conf.Credentials,
			"sdkInfo": map[string]interface{}{
				"implementation": "browser",
				"version":        sdkVersion,
				"userAgent":      userAgent,
				"hwConcurrency":  4,
			},
			"capabilitiesOffer": map[string]interface{}{
				"offerAnswerMode":         []string{"SEPARATE"},
				"initialSubscriberOffer":  []string{"ON_HELLO"},
				"dataChannelSharing":      []string{"TO_RTP"},
				"dataChannelVideoCodec":   []string{"UNIQUE_CODEC_FROM_TRACK_DESCRIPTION"},
				"slotsMode":              []string{"FROM_CONTROLLER"},
				"simulcastMode":          []string{"DISABLED"},
				"publisherSdpSemantics":  []string{"UNIFIED_PLAN"},
				"publisherVp9":           []string{"PUBLISH_VP9_ENABLED"},
				"svcMode":               []string{"SVC_MODE_L3T3_KEY"},
				"iceProtocol":            []string{"ALL"},
				"iceCandidateProtocol":   []string{"ALL"},
				"audioBitrateMode":       []string{"VARIABLE"},
				"sdpMLineOrder":          []string{"ANY"},
				"opusDtxMode":            []string{"ENABLED"},
				"audioRedMode":           []string{"DISABLED"},
				"publisherIceLiteRemote": []string{"SUPPORTED"},
				"videoEncoderConfig":     []string{"NO_CONFIG"},
			},
			"sdkInitializationId":    uuid.New().String(),
			"disablePublisher":       false,
			"disableSubscriber":      false,
			"disableSubscriberAudio": false,
		},
	}
}

// --- helpers ---

// extractMeetingID extracts the meeting ID from a Telemost URL or returns as-is.
// Supports: https://telemost.yandex.com/j/12345, https://telemost.yandex.ru/j/12345, 12345
func extractMeetingID(input string) string {
	input = strings.TrimSpace(input)
	// Handle full URLs.
	if strings.Contains(input, "/j/") {
		parts := strings.SplitN(input, "/j/", 2)
		if len(parts) == 2 {
			// Strip any query params or fragments.
			id := strings.SplitN(parts[1], "?", 2)[0]
			id = strings.SplitN(id, "#", 2)[0]
			return id
		}
	}
	return input
}

// IsTelemostLink returns true if the link looks like a Telemost meeting URL/ID.
func IsTelemostLink(link string) bool {
	link = strings.TrimSpace(link)
	if strings.Contains(link, "telemost.yandex") {
		return true
	}
	// All-digit IDs longer than 10 chars are likely Telemost meeting IDs.
	if len(link) > 10 && isAllDigits(link) {
		return true
	}
	return false
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
