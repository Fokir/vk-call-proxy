package client

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/provider"
	internalsignal "github.com/call-vpn/call-vpn/internal/signal"
	"github.com/call-vpn/call-vpn/internal/turn"
	"github.com/google/uuid"
)

// ReconnectConfig holds the configuration for the reconnect manager.
type ReconnectConfig struct {
	TargetConns     int
	AuthToken       string
	Fingerprint     []byte
	SessionID       uuid.UUID
	Logger          *slog.Logger
	OnFullReconnect func() // called when signaling dies or all connections are lost
}

// ReconnectManager monitors mux connection health and reconnects
// individual DTLS-over-TURN connections when they die.
// It handles per-connection reconnects when signaling is alive,
// and invokes OnFullReconnect when signaling dies.
type ReconnectManager struct {
	cfg       ReconnectConfig
	sigClient provider.SignalingClient
	mgr       *turn.Manager
	m         *mux.Mux
}

// NewReconnectManager creates a new ReconnectManager.
func NewReconnectManager(cfg ReconnectConfig, sig provider.SignalingClient, mgr *turn.Manager, m *mux.Mux) *ReconnectManager {
	return &ReconnectManager{cfg: cfg, sigClient: sig, mgr: mgr, m: m}
}

// Run starts the unified reconnect loop. It blocks until ctx is cancelled
// or a full reconnect is triggered via OnFullReconnect.
func (rm *ReconnectManager) Run(ctx context.Context) {
	logger := rm.cfg.Logger
	sigClient := rm.sigClient
	mgr := rm.mgr
	m := rm.m
	targetConns := rm.cfg.TargetConns

	// Local context: cancelled when Run returns,
	// so the ConnDied drain goroutine doesn't leak after full session reconnect.
	localCtx, localCancel := context.WithCancel(ctx)
	defer localCancel()

	ackCh, unsub := sigClient.Subscribe(internalsignal.WireConnOk, 8)
	defer unsub()

	minActive := targetConns / 2
	if minActive < 2 {
		minActive = 2
	}
	if minActive > targetConns {
		minActive = targetConns
	}

	const healthTimeout = 10 * time.Second

	// Wakeup signal: triggered on connection death for fast response.
	wakeup := make(chan struct{}, 1)
	triggerWakeup := func() {
		select {
		case wakeup <- struct{}{}:
		default:
		}
	}

	// Drain ConnDied and trigger wakeup.
	go func() {
		for {
			select {
			case <-localCtx.Done():
				return
			case idx, ok := <-m.ConnDied():
				if !ok {
					return
				}
				m.RemoveConn(idx)
				// Log allocation age if available.
				allocs := mgr.Allocations()
				var ageStr string
				if idx < len(allocs) && allocs[idx] != nil {
					ageStr = time.Since(allocs[idx].CreatedAt).Round(time.Second).String()
				}
				logger.Info("connection died", "index", idx, "allocation_age", ageStr)
				triggerWakeup()
			}
		}
	}()

	// Unified loop: check every 2s or on wakeup, maintain target connection count.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	const (
		normalMaxAttempts       = 5
		maxBackoff              = 30 * time.Second
		criticalMaxAttempts     = 10 // after this many consecutive critical failures, trigger full reconnect
	)
	var consecutiveCriticalFails int

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wakeup:
		}

		// If signaling is dead, per-conn reconnect is impossible -- trigger full session reconnect.
		if !sigClient.IsAlive() {
			logger.Warn("signaling connection dead, triggering full session reconnect")
			rm.cfg.OnFullReconnect()
			return // this reconnect manager instance exits; a new one starts after full reconnect
		}

		active := m.ActiveConns()
		healthy := m.IsHealthy(healthTimeout)
		if active >= targetConns && healthy {
			continue
		}

		critical := active < minActive || !healthy
		needed := targetConns - active
		if needed <= 0 {
			// All conns alive but unhealthy -- probe and force reconnect of 1.
			needed = 1
			m.ProbeConnections(3 * time.Second)
			logger.Warn("tunnel unhealthy, no pong received",
				"active", active, "healthy", healthy)
			// Wait for probe to detect dead connections.
			select {
			case <-ctx.Done():
				return
			case <-time.After(4 * time.Second):
			}
			active = m.ActiveConns()
			needed = targetConns - active
			if needed <= 0 {
				continue
			}
		}
		logger.Info("connections below target, reconnecting",
			"active", active, "target", targetConns, "needed", needed,
			"min_active", minActive, "critical", critical, "healthy", healthy)

		for i := 0; i < needed; i++ {
			m.BeginReconnect()
			var err error
			backoff := time.Second
			for attempt := 1; ; attempt++ {
				// Re-check signaling before each attempt.
				if !sigClient.IsAlive() {
					logger.Warn("signaling died during reconnect, triggering full session reconnect")
					m.EndReconnect()
					rm.cfg.OnFullReconnect()
					return
				}

				err = rm.reconnectOne(ctx, ackCh)
				if err == nil {
					consecutiveCriticalFails = 0
					break
				}

				if !critical && attempt >= normalMaxAttempts {
					logger.Warn("reconnect failed, will retry next cycle",
						"attempts", attempt, "err", err)
					break
				}

				// If all connections are dead and we keep failing, the server
				// likely closed the session. Trigger full reconnect to start fresh.
				if critical && m.ActiveConns() == 0 {
					consecutiveCriticalFails++
					if consecutiveCriticalFails >= criticalMaxAttempts {
						logger.Warn("too many critical reconnect failures with zero active connections, triggering full session reconnect",
							"consecutive_fails", consecutiveCriticalFails)
						m.EndReconnect()
						rm.cfg.OnFullReconnect()
						return
					}
				}

				logger.Warn("reconnect attempt failed",
					"attempt", attempt, "err", err, "critical", critical,
					"next_backoff", backoff)
				select {
				case <-ctx.Done():
					m.EndReconnect()
					return
				case <-time.After(backoff):
				}
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}

				// Re-check if still critical (other reconnects may have succeeded).
				critical = m.ActiveConns() < minActive || !m.IsHealthy(healthTimeout)
			}
			m.EndReconnect()
			if err != nil && !critical {
				break // normal mode: stop, wait for next tick
			}
		}
	}
}

// reconnectOne creates a single new TURN allocation, exchanges relay addresses
// via signaling, performs DTLS handshake, and adds the connection to the mux.
func (rm *ReconnectManager) reconnectOne(ctx context.Context, ackCh <-chan provider.SignalMessage) error {
	allocs, err := rm.mgr.Allocate(ctx, 1)
	if err != nil {
		return fmt.Errorf("allocate TURN: %w", err)
	}
	alloc := allocs[0]
	// On failure, close and remove the allocation to free the quota.
	// On success this is set to false so the allocation persists.
	closeAlloc := true
	defer func() {
		if closeAlloc {
			rm.mgr.RemoveAllocation(alloc)
		}
	}()
	myAddr := alloc.RelayAddr.String()

	// Send our new relay address to server.
	if err := rm.sigClient.SendPayload(ctx, internalsignal.WireConnNew, []byte(myAddr)); err != nil {
		return fmt.Errorf("send conn-new: %w", err)
	}

	// Wait for server's relay address (timeout 15s).
	var serverAddr string
	select {
	case msg, ok := <-ackCh:
		if !ok {
			return fmt.Errorf("ack channel closed")
		}
		payload, err := base64.StdEncoding.DecodeString(msg.Payload)
		if err != nil {
			return fmt.Errorf("decode ack payload: %w", err)
		}
		serverAddr = string(payload)
	case <-time.After(15 * time.Second):
		return fmt.Errorf("timeout waiting for server relay addr")
	case <-ctx.Done():
		return ctx.Err()
	}

	serverUDP, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return fmt.Errorf("resolve server addr: %w", err)
	}
	// Punch and DTLS dial.
	punchCtx, punchCancel := context.WithCancel(ctx)
	defer punchCancel()
	internaldtls.PunchRelay(alloc.RelayConn, serverUDP)
	go internaldtls.StartPunchLoop(punchCtx, alloc.RelayConn, serverUDP)
	time.Sleep(200 * time.Millisecond)

	reconnCtx, reconnCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reconnCancel()
	dtlsConn, _, err := internaldtls.DialOverTURN(reconnCtx, alloc.RelayConn, serverUDP, rm.cfg.Fingerprint)
	if err != nil {
		return fmt.Errorf("DialOverTURN: %w", err)
	}
	punchCancel()

	// Auth + session ID.
	if rm.cfg.AuthToken != "" {
		if err := mux.WriteAuthToken(dtlsConn, rm.cfg.AuthToken); err != nil {
			dtlsConn.Close()
			return fmt.Errorf("write auth token: %w", err)
		}
	}
	var sid [16]byte
	copy(sid[:], rm.cfg.SessionID[:])
	if err := mux.WriteSessionID(dtlsConn, sid); err != nil {
		dtlsConn.Close()
		return fmt.Errorf("write session id: %w", err)
	}

	closeAlloc = false // success: allocation is now in use by the MUX connection
	rm.m.AddConn(dtlsConn)
	rm.cfg.Logger.Info("reconnect: new connection added to MUX",
		"active", rm.m.ActiveConns(),
		"total", rm.m.TotalConns(),
	)
	return nil
}
