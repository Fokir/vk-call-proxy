package testrig

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SignalingServer is a mock WebSocket signaling server that emulates
// the VK Calls protocol for local testing.
type SignalingServer struct {
	Addr string // host:port after Start

	ln       net.Listener
	srv      *http.Server
	upgrader websocket.Upgrader

	mu    sync.Mutex
	rooms map[string]*room
	fault map[string]*faultConfig // roomID → fault injection
}

type faultConfig struct {
	latency  time.Duration
	dropRate float64 // 0..1
}

type room struct {
	mu           sync.Mutex
	participants map[string]*participant
}

type participant struct {
	peerID string
	conn   *websocket.Conn
	done   chan struct{}
}

// inbound command from client
type commandMsg struct {
	Command  string          `json:"command"`
	Sequence int             `json:"sequence,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// NewSignalingServer creates a mock signaling server listening on 127.0.0.1:0.
func NewSignalingServer() (*SignalingServer, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("testrig: listen: %w", err)
	}
	s := &SignalingServer{
		Addr:  ln.Addr().String(),
		ln:    ln,
		rooms: make(map[string]*room),
		fault: make(map[string]*faultConfig),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/call/", s.handleCall)
	s.srv = &http.Server{Handler: mux}
	go s.srv.Serve(ln) //nolint:errcheck
	return s, nil
}

// Close shuts down the server and all connections.
func (s *SignalingServer) Close() error {
	s.mu.Lock()
	for _, r := range s.rooms {
		r.mu.Lock()
		for _, p := range r.participants {
			close(p.done)
			p.conn.Close()
		}
		r.mu.Unlock()
	}
	s.mu.Unlock()
	return s.srv.Close()
}

// URL returns the WebSocket URL for a given room.
func (s *SignalingServer) URL(roomID string) string {
	return fmt.Sprintf("ws://%s/call/%s", s.Addr, roomID)
}

// SetLatency injects latency for all messages routed in the given room.
func (s *SignalingServer) SetLatency(roomID string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getFault(roomID).latency = d
}

// DropMessages sets the drop rate (0.0–1.0) for messages in the given room.
func (s *SignalingServer) DropMessages(roomID string, rate float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getFault(roomID).dropRate = rate
}

// KickParticipant forcefully disconnects a participant from a room.
func (s *SignalingServer) KickParticipant(roomID, peerID string) {
	s.mu.Lock()
	r := s.rooms[roomID]
	s.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	p, ok := r.participants[peerID]
	if ok {
		close(p.done)
		p.conn.Close()
		delete(r.participants, peerID)
	}
	r.mu.Unlock()
}

// KickAllInRoom forcefully disconnects all participants in a room.
func (s *SignalingServer) KickAllInRoom(roomID string) {
	s.mu.Lock()
	r := s.rooms[roomID]
	s.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	for id, p := range r.participants {
		close(p.done)
		p.conn.Close()
		delete(r.participants, id)
	}
	r.mu.Unlock()
}

func (s *SignalingServer) getFault(roomID string) *faultConfig {
	fc := s.fault[roomID]
	if fc == nil {
		fc = &faultConfig{}
		s.fault[roomID] = fc
	}
	return fc
}

func (s *SignalingServer) faultFor(roomID string) (latency time.Duration, dropRate float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc := s.fault[roomID]
	if fc == nil {
		return 0, 0
	}
	return fc.latency, fc.dropRate
}

func (s *SignalingServer) getOrCreateRoom(roomID string) *room {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rooms[roomID]
	if r == nil {
		r = &room{participants: make(map[string]*participant)}
		s.rooms[roomID] = r
	}
	return r
}

func (s *SignalingServer) handleCall(w http.ResponseWriter, r *http.Request) {
	// Extract roomID from path: /call/{roomID}
	// Strip query params — VK client adds random params.
	path := r.URL.Path
	roomID := strings.TrimPrefix(path, "/call/")
	if roomID == "" || roomID == path {
		http.Error(w, "missing room ID", http.StatusBadRequest)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	peerID := randomPeerID()
	p := &participant{
		peerID: peerID,
		conn:   conn,
		done:   make(chan struct{}),
	}

	rm := s.getOrCreateRoom(roomID)

	// Notify existing participants about the new one.
	rm.mu.Lock()
	for _, existing := range rm.participants {
		joined, _ := json.Marshal(map[string]string{
			"notification":  "participant-joined",
			"participantId": peerID,
		})
		existing.conn.WriteMessage(websocket.TextMessage, joined) //nolint:errcheck
	}
	rm.participants[peerID] = p
	rm.mu.Unlock()

	// Send connection notification to the new participant.
	connMsg, _ := json.Marshal(map[string]string{
		"notification": "connection",
		"peerId":       peerID,
	})
	conn.WriteMessage(websocket.TextMessage, connMsg) //nolint:errcheck

	// Start keepalive pinger.
	go s.pingLoop(p)

	// Read loop.
	s.readLoop(roomID, p, rm)
}

func (s *SignalingServer) pingLoop(p *participant) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			if err := p.conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
				return
			}
		}
	}
}

func (s *SignalingServer) readLoop(roomID string, p *participant, rm *room) {
	defer func() {
		rm.mu.Lock()
		delete(rm.participants, p.peerID)
		rm.mu.Unlock()
		p.conn.Close()
	}()

	for {
		msgType, data, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		if msgType != websocket.TextMessage {
			continue
		}

		// Handle "pong" keepalive reply — just ignore it.
		if string(data) == "pong" {
			continue
		}

		var cmd commandMsg
		if err := json.Unmarshal(data, &cmd); err != nil {
			continue
		}

		switch cmd.Command {
		case "custom-data":
			s.routeCustomData(roomID, p.peerID, cmd.Data, rm)
		}
	}
}

func (s *SignalingServer) routeCustomData(roomID, senderID string, data json.RawMessage, rm *room) {
	latency, dropRate := s.faultFor(roomID)

	if latency > 0 {
		time.Sleep(latency)
	}

	// Build notification.
	notification, _ := json.Marshal(struct {
		Notification  string          `json:"notification"`
		ParticipantID string          `json:"participantId"`
		Data          json.RawMessage `json:"data"`
	}{
		Notification:  "custom-data",
		ParticipantID: senderID,
		Data:          data,
	})

	rm.mu.Lock()
	defer rm.mu.Unlock()
	for id, p := range rm.participants {
		if id == senderID {
			continue
		}
		if dropRate > 0 && rand.Float64() < dropRate { //nolint:gosec
			continue
		}
		p.conn.WriteMessage(websocket.TextMessage, notification) //nolint:errcheck
	}
}

func randomPeerID() string {
	return fmt.Sprintf("%d", 10000+rand.Intn(90000)) //nolint:gosec
}
