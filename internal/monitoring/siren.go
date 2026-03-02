package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// AlertLevel indicates severity.
type AlertLevel string

const (
	AlertInfo     AlertLevel = "info"
	AlertWarning  AlertLevel = "warning"
	AlertCritical AlertLevel = "critical"
)

// Alert represents a monitoring event.
type Alert struct {
	Level     AlertLevel `json:"level"`
	Component string     `json:"component"`
	Message   string     `json:"message"`
	Timestamp time.Time  `json:"timestamp"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Siren handles alerting via Slack webhooks and email.
type Siren struct {
	slackWebhook string
	logger       *slog.Logger
	mu           sync.Mutex
	history      []Alert
	maxHistory   int
}

// New creates a Siren instance configured from environment variables.
func New(logger *slog.Logger) *Siren {
	return &Siren{
		slackWebhook: os.Getenv("SIREN_SLACK_WEBHOOK"),
		logger:       logger,
		maxHistory:   1000,
	}
}

// Send dispatches an alert to all configured channels.
func (s *Siren) Send(ctx context.Context, alert Alert) {
	alert.Timestamp = time.Now()

	s.mu.Lock()
	s.history = append(s.history, alert)
	if len(s.history) > s.maxHistory {
		s.history = s.history[len(s.history)-s.maxHistory:]
	}
	s.mu.Unlock()

	s.logger.Log(ctx, levelToSlog(alert.Level), alert.Message,
		"component", alert.Component,
		"level", alert.Level,
	)

	if s.slackWebhook != "" {
		go s.sendSlack(ctx, alert)
	}
}

// History returns recent alerts.
func (s *Siren) History() []Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Alert, len(s.history))
	copy(out, s.history)
	return out
}

func (s *Siren) sendSlack(ctx context.Context, alert Alert) {
	text := fmt.Sprintf("[%s] *%s* — %s", alert.Level, alert.Component, alert.Message)
	payload, _ := json.Marshal(map[string]string{"text": text})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.slackWebhook, bytes.NewReader(payload))
	if err != nil {
		s.logger.Warn("slack request creation failed", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.logger.Warn("slack notification failed", "err", err)
		return
	}
	resp.Body.Close()
}

func levelToSlog(level AlertLevel) slog.Level {
	switch level {
	case AlertCritical:
		return slog.LevelError
	case AlertWarning:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// AlertTURNAuthFailure sends a TURN auth failure alert.
func (s *Siren) AlertTURNAuthFailure(ctx context.Context, err error) {
	s.Send(ctx, Alert{
		Level:     AlertCritical,
		Component: "turn",
		Message:   fmt.Sprintf("TURN authentication failed: %v", err),
	})
}

// AlertPacketLoss sends a packet loss alert.
func (s *Siren) AlertPacketLoss(ctx context.Context, lossPercent float64, threshold float64) {
	s.Send(ctx, Alert{
		Level:     AlertWarning,
		Component: "mux",
		Message:   fmt.Sprintf("Packet loss %.1f%% exceeds threshold %.1f%%", lossPercent, threshold),
		Metadata:  map[string]any{"loss_percent": lossPercent, "threshold": threshold},
	})
}

// AlertDisconnect sends a client/server disconnect alert.
func (s *Siren) AlertDisconnect(ctx context.Context, peerID string) {
	s.Send(ctx, Alert{
		Level:     AlertWarning,
		Component: "connection",
		Message:   fmt.Sprintf("Peer disconnected: %s", peerID),
	})
}

// AlertTunnelDegradation sends a tunnel quality degradation alert.
func (s *Siren) AlertTunnelDegradation(ctx context.Context, activeConns, totalConns int) {
	s.Send(ctx, Alert{
		Level:     AlertWarning,
		Component: "mux",
		Message:   fmt.Sprintf("Tunnel degraded: %d/%d connections active", activeConns, totalConns),
		Metadata:  map[string]any{"active": activeConns, "total": totalConns},
	})
}
