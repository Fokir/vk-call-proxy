package testrig

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
)

// mockService implements provider.Service backed by local TURN + mock signaling.
type mockService struct {
	name      string
	turnSrvs  []*TURNServer
	sigURL    string // signaling server base URL like "ws://127.0.0.1:XXXXX"
	roomID    string
	mu        sync.Mutex
	callIndex int // for round-robin TURN selection
}

var _ provider.Service = (*mockService)(nil)

func (s *mockService) Name() string { return s.name }

func (s *mockService) FetchJoinInfo(ctx context.Context) (*provider.JoinInfo, error) {
	if len(s.turnSrvs) == 0 {
		return nil, fmt.Errorf("testrig: no TURN servers configured")
	}

	s.mu.Lock()
	idx := s.callIndex % len(s.turnSrvs)
	s.callIndex++
	s.mu.Unlock()

	primary := s.turnSrvs[idx]
	host, port, _ := net.SplitHostPort(primary.Addr)

	// Collect all TURN servers for Manager round-robin.
	servers := make([]provider.TURNServer, len(s.turnSrvs))
	for i, ts := range s.turnSrvs {
		h, p, _ := net.SplitHostPort(ts.Addr)
		servers[i] = provider.TURNServer{Host: h, Port: p}
	}

	return &provider.JoinInfo{
		Credentials: provider.Credentials{
			Username: primary.Username,
			Password: primary.Password,
			Host:     host,
			Port:     port,
			Servers:  servers,
		},
		WSEndpoint: s.sigURL + "/call/" + s.roomID,
		ConvID:     "testrig-" + s.roomID,
	}, nil
}

func (s *mockService) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	info, err := s.FetchJoinInfo(ctx)
	if err != nil {
		return nil, err
	}
	return &info.Credentials, nil
}

func (s *mockService) ConnectSignaling(ctx context.Context, info *provider.JoinInfo, logger *slog.Logger) (provider.SignalingClient, error) {
	return vk.ConnectSignaling(ctx, info.WSEndpoint, info.DeviceIdx, logger)
}
