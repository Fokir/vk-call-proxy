package testrig

import (
	"fmt"
	"testing"
	"time"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// Options configures a TestRig.
type Options struct {
	TURNServers int  // number of local TURN servers (default 1)
	Calls       int  // number of signaling rooms (default 1)
	UseTCP      bool // TCP TURN (default false = UDP)
}

// TestRig orchestrates local TURN servers, a mock signaling server, and
// provider.Service instances for integration tests.
type TestRig struct {
	t        *testing.T
	turnSrvs []*TURNServer
	sigSrv   *SignalingServer
	services []*mockService
	opts     Options
}

// New creates a TestRig with the given options. It registers a cleanup
// function via t.Cleanup so callers don't need to call Close manually.
func New(t *testing.T, opts Options) *TestRig {
	t.Helper()
	if opts.TURNServers <= 0 {
		opts.TURNServers = 1
	}
	if opts.Calls <= 0 {
		opts.Calls = 1
	}

	r := &TestRig{t: t, opts: opts}

	for i := range opts.TURNServers {
		r.turnSrvs = append(r.turnSrvs, newTURNServer(t, i))
	}

	var err error
	r.sigSrv, err = NewSignalingServer()
	if err != nil {
		t.Fatalf("testrig: start signaling server: %v", err)
	}

	sigURL := fmt.Sprintf("ws://%s", r.sigSrv.Addr)

	for i := range opts.Calls {
		r.services = append(r.services, &mockService{
			name:      "testrig",
			turnSrvs:  r.turnSrvs,
			sigURL:    sigURL,
			roomID:    fmt.Sprintf("room%d", i),
			callIndex: i,
		})
	}

	t.Cleanup(r.Close)
	return r
}

// Service returns the provider.Service for the given call index.
func (r *TestRig) Service(callIndex int) provider.Service { return r.services[callIndex] }

// Credentials returns TURN credentials for the given TURN server index.
func (r *TestRig) Credentials(turnIndex int) *provider.Credentials {
	return r.turnSrvs[turnIndex].Credentials()
}

// TURNAddr returns the listen address of the given TURN server.
func (r *TestRig) TURNAddr(index int) string { return r.turnSrvs[index].Addr }

// --- Fault injection ---

// KillTURN closes the TURN server at the given index.
func (r *TestRig) KillTURN(index int) { r.turnSrvs[index].Close() }

// RestartTURN replaces the TURN server at the given index with a fresh one.
func (r *TestRig) RestartTURN(index int) { r.turnSrvs[index] = newTURNServer(r.t, index) }

// KillSignaling forcefully disconnects all participants in the room for the given call.
func (r *TestRig) KillSignaling(callIndex int) {
	r.sigSrv.KickAllInRoom(r.services[callIndex].roomID)
}

// InjectLatency adds artificial latency to signaling messages in the given call's room.
func (r *TestRig) InjectLatency(callIndex int, d time.Duration) {
	r.sigSrv.SetLatency(r.services[callIndex].roomID, d)
}

// DropMessages sets a message drop rate (0.0-1.0) for the given call's room.
func (r *TestRig) DropMessages(callIndex int, rate float64) {
	r.sigSrv.DropMessages(r.services[callIndex].roomID, rate)
}

// Close shuts down all TURN servers and the signaling server.
func (r *TestRig) Close() {
	for _, ts := range r.turnSrvs {
		ts.Close()
	}
	r.sigSrv.Close()
}
