// Package vk implements provider.Service for VK Calls.
// It fetches TURN credentials via the VK/OK authentication chain
// and connects to VK WebSocket signaling for relay-to-relay mode.
package vk

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/call-vpn/call-vpn/internal/provider"
)

// Service implements provider.Service for VK Calls.
type Service struct {
	callLink string
	captcha  provider.CaptchaSolver
}

// Option configures a VK Service.
type Option func(*Service)

// WithCaptchaSolver sets a captcha solver for automatic captcha resolution.
func WithCaptchaSolver(s provider.CaptchaSolver) Option {
	return func(svc *Service) { svc.captcha = s }
}

// Compile-time checks.
var _ provider.Service = (*Service)(nil)
var _ provider.TokenAuthProvider = (*Service)(nil)

// NewService creates a VK call service provider.
func NewService(callLink string, opts ...Option) *Service {
	s := &Service{callLink: callLink}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *Service) Name() string { return "vk" }

// FetchCredentials obtains anonymous TURN credentials from VK using the
// 4-step authentication chain. Each call generates a fresh anonymous identity.
func (s *Service) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	ji, err := s.FetchJoinInfo(ctx)
	if err != nil {
		return nil, err
	}
	creds := ji.Credentials
	return &creds, nil
}

// FetchJoinInfo performs the VK authentication chain via auth.lua and returns
// TURN credentials, WebSocket endpoint, and conversation info.
func (s *Service) FetchJoinInfo(ctx context.Context) (*provider.JoinInfo, error) {
	mgr := activeScripts.Load()
	if mgr == nil {
		return nil, fmt.Errorf("scripts manager not initialized")
	}
	return runLuaAuth(ctx, mgr, s.captcha, s.callLink, provider.RandomDisplayName(), "")
}

// FetchJoinInfoWithToken implements provider.TokenAuthProvider.
// Uses the authorized flow via auth.lua: resolve OK auth_token → auth.anonymLogin(v3) → joinConference.
func (s *Service) FetchJoinInfoWithToken(ctx context.Context, token string) (*provider.JoinInfo, error) {
	mgr := activeScripts.Load()
	if mgr == nil {
		return nil, fmt.Errorf("scripts manager not initialized")
	}
	return runLuaAuth(ctx, mgr, s.captcha, s.callLink, provider.RandomDisplayName(), token)
}

// ConnectSignaling connects to VK WebSocket signaling.
func (s *Service) ConnectSignaling(ctx context.Context, info *provider.JoinInfo, logger *slog.Logger) (provider.SignalingClient, error) {
	return ConnectSignaling(ctx, info.WSEndpoint, info.DeviceIdx, logger)
}
