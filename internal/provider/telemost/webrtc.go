package telemost

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	sdpTimeout    = 15 * time.Second
	dcOpenTimeout = 120 * time.Second
	// Interval for sending keepalive VP8 frames to keep the SFU forwarding active.
	// 40ms = 25 fps — совпадает с частотой официального web-клиента и
	// whitelist-bypass. Более редкие кадры (например 100ms = 10fps) могут
	// вызывать reshape/drop на стороне SFU, что заметно бьёт по стабильности.
	videoKeepAliveInterval = 40 * time.Millisecond
)

// WebRTCTransport manages publisher and subscriber PeerConnections
// through a Goloom SFU signaling session.
type WebRTCTransport struct {
	publisher  *webrtc.PeerConnection
	subscriber *webrtc.PeerConnection
	goloom     *GoloomClient
	logger     *slog.Logger
	obfKey     [32]byte // XOR obfuscation key for VP8 payload masking

	pubVideo *webrtc.TrackLocalStaticSample // publisher video track for data
	rtpConn  *RTPConn                       // RTP-based data transport

	subReady chan struct{} // closed when subscriber video track is received

	mu     sync.Mutex
	closed bool
}

// EstablishWebRTC creates WebRTC PeerConnections through the Goloom SFU
// and returns a bidirectional RTPConn for VPN data.
// Blocks until the subscriber receives a video track from another participant.
func EstablishWebRTC(ctx context.Context, goloom *GoloomClient, logger *slog.Logger, obfKey [32]byte) (*RTPConn, error) {
	t, err := SetupWebRTC(ctx, goloom, logger, obfKey, "vpn-peer")
	if err != nil {
		return nil, err
	}

	if err := t.WaitReady(ctx); err != nil {
		t.Close()
		return nil, err
	}

	return t.rtpConn, nil
}

// SetupWebRTC creates WebRTC PeerConnections, sends updateMe/setSlots,
// and starts keepalive. Returns immediately without waiting for subscriber track.
// The name parameter sets the participant's display name in the conference.
func SetupWebRTC(ctx context.Context, goloom *GoloomClient, logger *slog.Logger, obfKey [32]byte, name string) (*WebRTCTransport, error) {
	iceServers := goloom.ICEServers()

	api := webrtc.NewAPI()

	t := &WebRTCTransport{
		goloom:   goloom,
		logger:   logger,
		obfKey:   obfKey,
		subReady: make(chan struct{}),
	}

	// The SFU sends subscriberSdpOffer immediately after hello
	// (capability: initialSubscriberOffer=ON_HELLO), so handle it first.
	if err := t.setupSubscriber(ctx, api, iceServers); err != nil {
		t.Close()
		return nil, fmt.Errorf("setup subscriber: %w", err)
	}

	// Then setup publisher (send our offer, get answer).
	if err := t.setupPublisher(ctx, api, iceServers); err != nil {
		t.Close()
		return nil, fmt.Errorf("setup publisher: %w", err)
	}

	// Tell SFU we're sending audio and video to activate media forwarding.
	// Both updateMe and updatePublisherTrackDescription must be sent together.
	if err := goloom.SendUpdateMe(name, true, true); err != nil {
		t.Close()
		return nil, fmt.Errorf("send updateMe: %w", err)
	}
	if err := goloom.SendPublisherTrackDescription(true, true); err != nil {
		t.Close()
		return nil, fmt.Errorf("send publisherTrackDescription: %w", err)
	}

	// Tell SFU the video slot layout we want to receive.
	if err := goloom.SendSetSlots(); err != nil {
		t.Close()
		return nil, fmt.Errorf("send setSlots: %w", err)
	}

	// Start routing server ICE candidates to the correct PeerConnection.
	go t.routeServerICE(ctx)

	// Start sending VP8 keepalive frames so the SFU detects active video.
	go t.videoKeepAlive(ctx)

	// Handle subscriber re-negotiation (SFU sends new offers when video becomes available).
	go t.subscriberRenegotiation(ctx)

	return t, nil
}

// WaitReady blocks until the subscriber receives a video track from another participant.
func (t *WebRTCTransport) WaitReady(ctx context.Context) error {
	t.logger.Info("waiting for subscriber video track")

	select {
	case <-t.subReady:
		t.logger.Info("subscriber video track active")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for subscriber video track: %w", ctx.Err())
	case <-time.After(dcOpenTimeout):
		return fmt.Errorf("timeout waiting for subscriber video track")
	}
}

// RTPConn returns the RTP connection for reading/writing VPN data.
func (t *WebRTCTransport) RTPConn() *RTPConn {
	return t.rtpConn
}

// ResetForPin resets subscriber readiness and SSRC lock after PinToPeer.
// This is needed when multiple participants from the same side are in the
// conference — the subscriber may have locked to the wrong peer's SSRC
// before pinning narrowed the SFU's video forwarding.
func (t *WebRTCTransport) ResetForPin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Don't reset subReady — the SFU reuses the existing VP8 track after pinning.
	// Just clear SSRC lock so the RTPConn re-locks to the pinned peer's SSRC.
	if t.rtpConn != nil {
		t.rtpConn.ResetSSRC()
	}
}

func (t *WebRTCTransport) setupPublisher(ctx context.Context, api *webrtc.API, iceServers []webrtc.ICEServer) error {
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		return err
	}
	t.publisher = pc

	// Add silent audio track (SFU expects audio from publisher).
	// Audio как data channel невозможен: Goloom SFU транскодирует Opus,
	// теряя произвольный payload (проверено MVP-экспериментом 2026-04-15).
	if err := addSilentAudioTrack(pc); err != nil {
		t.logger.Debug("failed to add silent audio track", "err", err)
	}

	// Add video track for VPN data transport.
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8},
		"video", "vpn-video",
	)
	if err != nil {
		return fmt.Errorf("create video track: %w", err)
	}
	videoSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return fmt.Errorf("add video track: %w", err)
	}
	t.pubVideo = videoTrack

	// Drain RTCP feedback from SFU for the video track.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := videoSender.Read(buf); err != nil {
				return
			}
		}
	}()

	// Create RTPConn using this video track.
	t.rtpConn = NewRTPConn(videoTrack, t.obfKey)

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		t.logger.Info("publisher ICE state", "state", state.String())
	})

	// Buffer ICE candidates — send only after SDP exchange completes.
	var iceMu sync.Mutex
	var bufferedCandidates []webrtc.ICECandidate
	sdpDone := make(chan struct{})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		iceMu.Lock()
		select {
		case <-sdpDone:
			iceMu.Unlock()
			t.sendICECandidate(c, "PUBLISHER")
		default:
			bufferedCandidates = append(bufferedCandidates, *c)
			iceMu.Unlock()
		}
	})

	// Create offer and set local description.
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("create offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	// Send offer to Goloom, get answer.
	t.logger.Debug("publisher SDP offer", "sdp", offer.SDP)
	sdpCtx, cancel := context.WithTimeout(ctx, sdpTimeout)
	defer cancel()

	answerSDP, err := t.goloom.SendPublisherOffer(sdpCtx, offer.SDP)
	if err != nil {
		return fmt.Errorf("publisher SDP exchange: %w", err)
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answerSDP,
	}
	if err := pc.SetRemoteDescription(answer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	// Flush buffered ICE candidates.
	close(sdpDone)
	iceMu.Lock()
	for _, c := range bufferedCandidates {
		t.sendICECandidate(&c, "PUBLISHER")
	}
	bufferedCandidates = nil
	iceMu.Unlock()

	t.logger.Info("publisher SDP exchange complete")
	return nil
}

func (t *WebRTCTransport) setupSubscriber(ctx context.Context, api *webrtc.API, iceServers []webrtc.ICEServer) error {
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		return err
	}
	t.subscriber = pc

	// Handle incoming tracks from the SFU.
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		t.logger.Info("subscriber received track",
			"kind", track.Kind().String(),
			"codec", track.Codec().MimeType,
			"ssrc", uint32(track.SSRC()),
		)
		// Only handle video tracks — that's where VPN data arrives.
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			t.rtpConn.HandleTrack(track)
			select {
			case <-t.subReady:
			default:
				close(t.subReady)
			}
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		t.logger.Debug("subscriber received DataChannel", "label", dc.Label())
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		t.logger.Info("subscriber ICE state", "state", state.String())
	})

	// Buffer ICE candidates.
	var iceMu sync.Mutex
	var bufferedCandidates []webrtc.ICECandidate
	sdpDone := make(chan struct{})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		iceMu.Lock()
		select {
		case <-sdpDone:
			iceMu.Unlock()
			t.sendICECandidate(c, "SUBSCRIBER")
		default:
			bufferedCandidates = append(bufferedCandidates, *c)
			iceMu.Unlock()
		}
	})

	// Wait for subscriber SDP offer from SFU.
	sdpCtx, cancel := context.WithTimeout(ctx, sdpTimeout)
	defer cancel()

	offerSDP, err := t.goloom.WaitSubscriberOffer(sdpCtx)
	if err != nil {
		return fmt.Errorf("wait subscriber offer: %w", err)
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerSDP,
	}
	if err := pc.SetRemoteDescription(offer); err != nil {
		return fmt.Errorf("set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local description: %w", err)
	}

	if err := t.goloom.SendSubscriberAnswer(answer.SDP); err != nil {
		return fmt.Errorf("send subscriber answer: %w", err)
	}

	// Flush buffered ICE candidates.
	close(sdpDone)
	iceMu.Lock()
	for _, c := range bufferedCandidates {
		t.sendICECandidate(&c, "SUBSCRIBER")
	}
	bufferedCandidates = nil
	iceMu.Unlock()

	t.logger.Info("subscriber SDP exchange complete")
	return nil
}

// subscriberRenegotiation handles subsequent subscriberSdpOffer messages from the SFU.
// The SFU sends a new offer when video tracks become available (e.g., another participant
// starts sending video). We must re-negotiate to accept those tracks.
func (t *WebRTCTransport) subscriberRenegotiation(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.closeCh():
			return
		case offerSDP, ok := <-t.goloom.WaitSubscriberOfferCh():
			if !ok {
				return
			}
			t.logger.Info("subscriber re-negotiation: new SDP offer from SFU")

			offer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  offerSDP,
			}
			if err := t.subscriber.SetRemoteDescription(offer); err != nil {
				t.logger.Error("subscriber re-negotiation: set remote description", "err", err)
				continue
			}

			answer, err := t.subscriber.CreateAnswer(nil)
			if err != nil {
				t.logger.Error("subscriber re-negotiation: create answer", "err", err)
				continue
			}
			if err := t.subscriber.SetLocalDescription(answer); err != nil {
				t.logger.Error("subscriber re-negotiation: set local description", "err", err)
				continue
			}

			if err := t.goloom.SendSubscriberAnswer(answer.SDP); err != nil {
				t.logger.Error("subscriber re-negotiation: send answer", "err", err)
				continue
			}
			t.logger.Info("subscriber re-negotiation complete")
		}
	}
}

func (t *WebRTCTransport) closeCh() <-chan struct{} {
	// Return a channel that's closed when transport is closed.
	// We reuse the rtpConn's closeCh if available.
	if t.rtpConn != nil {
		return t.rtpConn.closeCh
	}
	ch := make(chan struct{})
	return ch
}

// videoKeepAlive sends minimal VP8 keyframes to keep the SFU aware that we're
// actively sending video. Without actual media flowing, the SFU won't assign
// our video to other subscribers' slots.
func (t *WebRTCTransport) videoKeepAlive(ctx context.Context) {
	// Wait for publisher ICE to connect before sending.
	for {
		if t.closed {
			return
		}
		if t.publisher != nil && t.publisher.ICEConnectionState() == webrtc.ICEConnectionStateConnected {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.logger.Debug("videoKeepAlive: publisher ICE connected, starting VP8 frames")

	// Готовим пустой keepalive-кадр один раз: с частотой 25 fps пересчитывать
	// XOR-обфускацию каждые 40ms — лишний CPU. Сам кадр stateless
	// (не содержит seq/pktSeq), безопасно повторять.
	keepAliveFrame := buildVP8Frame(nil, t.obfKey)

	ticker := time.NewTicker(videoKeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if t.closed {
				return
			}
			t.pubVideo.WriteSample(media.Sample{
				Data:     keepAliveFrame,
				Duration: videoKeepAliveInterval,
			})
		}
	}
}

func (t *WebRTCTransport) sendICECandidate(c *webrtc.ICECandidate, target string) {
	init := c.ToJSON()
	mid := ""
	if init.SDPMid != nil {
		mid = *init.SDPMid
	}
	if err := t.goloom.SendICECandidate(init.Candidate, mid, target); err != nil {
		t.logger.Debug("send ICE candidate failed", "target", target, "err", err)
	}
}

// routeServerICE routes incoming ICE candidates from Goloom to the correct PeerConnection.
func (t *WebRTCTransport) routeServerICE(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ic, ok := <-t.goloom.RecvICECandidate():
			if !ok {
				return
			}
			candidate := webrtc.ICECandidateInit{
				Candidate: ic.Candidate,
			}
			if ic.SDPMid != "" {
				mid := ic.SDPMid
				candidate.SDPMid = &mid
			}

			var pc *webrtc.PeerConnection
			switch ic.Target {
			case "PUBLISHER":
				pc = t.publisher
			case "SUBSCRIBER":
				pc = t.subscriber
			default:
				continue
			}
			if pc == nil {
				continue
			}
			if err := pc.AddICECandidate(candidate); err != nil {
				t.logger.Debug("add ICE candidate failed", "target", ic.Target, "err", err)
			}
		}
	}
}

// Close tears down both PeerConnections.
func (t *WebRTCTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.rtpConn != nil {
		t.rtpConn.Close()
	}
	if t.publisher != nil {
		t.publisher.Close()
	}
	if t.subscriber != nil {
		t.subscriber.Close()
	}
	return nil
}

// addSilentAudioTrack adds a dummy Opus audio track to the PeerConnection.
// SFU требует audio от publisher'ов для активации media forwarding.
// Использовать audio как канал данных невозможно: Goloom транскодирует Opus
// (проверено MVP-экспериментом 2026-04-15 — 205-байтный payload с magic
// превращался в ~25-байтный «нормализованный» Opus без нашего content).
func addSilentAudioTrack(pc *webrtc.PeerConnection) error {
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus},
		"audio", "vpn-silent",
	)
	if err != nil {
		return err
	}
	_, err = pc.AddTrack(track)
	return err
}
