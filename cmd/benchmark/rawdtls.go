package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/provider/vk"
	"github.com/call-vpn/call-vpn/internal/turn"
)

func benchRawDTLS(ctx context.Context, cfg *config, logger *slog.Logger, sig provider.SignalingClient, isSender bool) ([]BenchResult, error) {
	var results []BenchResult
	for run := 0; run < cfg.runs; run++ {
		logger.Info("raw-dtls run", "run", run+1, "of", cfg.runs)

		r, err := runOneRawDTLS(ctx, cfg, logger, sig, isSender, run+1)
		if err != nil {
			logger.Warn("raw-dtls run failed", "run", run+1, "err", err)
		} else {
			results = append(results, r)
		}

		// sync between runs — ensure both peers finished before next run
		if err := syncPeers(ctx, sig, fmt.Sprintf("raw-done-%d", run)); err != nil {
			return results, err
		}
	}
	return results, nil
}

func runOneRawDTLS(ctx context.Context, cfg *config, logger *slog.Logger, sig provider.SignalingClient, isSender bool, run int) (BenchResult, error) {
	setupStart := time.Now()

	// allocate TURN
	svc := vk.NewService(cfg.callLink)
	mgr := turn.NewManager(svc, cfg.useTCP, logger)
	allocs, err := mgr.Allocate(ctx, cfg.numConns)
	if err != nil {
		return BenchResult{}, fmt.Errorf("allocate: %w", err)
	}
	// manual cleanup — NOT deferred, because we must keep connections alive
	// until data transfer is confirmed by peer

	ourAddrs := make([]string, len(allocs))
	for i, a := range allocs {
		ourAddrs[i] = a.RelayAddr.String()
	}

	// exchange relay addresses
	role, skipRole := "client", "client"
	if !isSender {
		role, skipRole = "server", "server"
	}

	sendCtx, sendCancel := context.WithCancel(ctx)
	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		for {
			_ = sig.SendRelayAddrs(sendCtx, ourAddrs, role)
			select {
			case <-sendCtx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()

	peerAddrs, _, err := sig.RecvRelayAddrs(ctx, skipRole)
	if err != nil {
		sendCancel()
		<-sendDone
		mgr.CloseAll()
		return BenchResult{}, fmt.Errorf("recv addrs: %w", err)
	}

	// stop sending relay addrs after 5s, but wait for goroutine to finish
	go func() {
		time.Sleep(5 * time.Second)
		sendCancel()
	}()

	// DTLS handshakes in parallel
	pairCount := min(len(allocs), len(peerAddrs))
	type handshakeResult struct {
		conn    net.Conn
		cleanup context.CancelFunc
		err     error
	}
	ch := make(chan handshakeResult, pairCount)
	punchCtx, punchCancel := context.WithCancel(ctx)

	for i := 0; i < pairCount; i++ {
		peerUDP, err := net.ResolveUDPAddr("udp", peerAddrs[i])
		if err != nil {
			ch <- handshakeResult{err: err}
			continue
		}
		go func(relayConn net.PacketConn, addr *net.UDPAddr) {
			internaldtls.PunchRelay(relayConn, addr)
			go internaldtls.StartPunchLoop(punchCtx, relayConn, addr)
			time.Sleep(500 * time.Millisecond)

			var conn net.Conn
			var cleanup context.CancelFunc
			var herr error
			if isSender {
				conn, cleanup, herr = internaldtls.DialOverTURN(ctx, relayConn, addr)
			} else {
				conn, cleanup, herr = internaldtls.AcceptOverTURN(ctx, relayConn, addr)
			}
			ch <- handshakeResult{conn, cleanup, herr}
		}(allocs[i].RelayConn, peerUDP)
	}

	var dtlsConns []io.ReadWriteCloser
	var cleanups []context.CancelFunc
	for i := 0; i < pairCount; i++ {
		r := <-ch
		if r.err != nil {
			logger.Warn("DTLS handshake failed", "err", r.err)
			continue
		}
		dtlsConns = append(dtlsConns, r.conn)
		cleanups = append(cleanups, r.cleanup)
	}
	punchCancel()

	// wait for relay addr goroutine to finish before any signaling
	sendCancel()
	<-sendDone

	if len(dtlsConns) == 0 {
		mgr.CloseAll()
		return BenchResult{}, fmt.Errorf("no DTLS connections established")
	}
	logger.Info("DTLS established", "connections", len(dtlsConns))

	// auth + session ID
	sessionID := uuid.New()
	var validConns []io.ReadWriteCloser
	for _, conn := range dtlsConns {
		if isSender {
			if cfg.token != "" {
				if err := mux.WriteAuthToken(conn, cfg.token); err != nil {
					logger.Warn("write auth failed", "err", err)
					continue
				}
			}
			var sid [16]byte
			copy(sid[:], sessionID[:])
			if err := mux.WriteSessionID(conn, sid); err != nil {
				logger.Warn("write session id failed", "err", err)
				continue
			}
		} else {
			if cfg.token != "" {
				if err := mux.ValidateAuthToken(conn, cfg.token); err != nil {
					logger.Warn("auth validation failed", "err", err)
					continue
				}
			}
			if _, err := mux.ReadSessionID(conn); err != nil {
				logger.Warn("read session id failed", "err", err)
				continue
			}
		}
		validConns = append(validConns, conn)
	}

	if len(validConns) == 0 {
		for _, c := range cleanups {
			c()
		}
		mgr.CloseAll()
		return BenchResult{}, fmt.Errorf("no connections passed auth")
	}

	// create MUX
	m := mux.New(logger, validConns...)

	sessCtx, sessCancel := context.WithCancel(ctx)

	if !isSender {
		m.EnableStreamAccept(16)
	}
	go m.DispatchLoop(sessCtx)
	go m.StartPingLoop(sessCtx, 30*time.Second)

	setupTime := time.Since(setupStart)

	// data transfer
	totalBytes := int64(cfg.sizeMB) * 1024 * 1024
	result := BenchResult{
		Mode:      "raw-dtls",
		Run:       run,
		SetupTime: setupTime,
	}

	if isSender {
		stream, err := m.OpenStream(1)
		if err != nil {
			sessCancel()
			m.Close()
			for _, c := range cleanups {
				c()
			}
			mgr.CloseAll()
			return result, fmt.Errorf("open stream: %w", err)
		}

		payload := generatePayload(mux.MaxFramePayload)
		start := time.Now()
		var sent int64
		for sent < totalBytes {
			chunk := payload
			if rem := totalBytes - sent; rem < int64(len(chunk)) {
				chunk = chunk[:rem]
			}
			n, err := stream.Write(chunk)
			if err != nil {
				sessCancel()
				m.Close()
				for _, c := range cleanups {
					c()
				}
				mgr.CloseAll()
				return result, fmt.Errorf("write at %d/%d: %w", sent, totalBytes, err)
			}
			sent += int64(n)

			// Throttle to prevent pion/turn readCh overflow (1024 buffer).
			time.Sleep(5 * time.Millisecond)
		}
		result.BytesSent = sent
		logger.Info("sender: data written, waiting for receiver ack...", "bytes", sent)

		ackTag := fmt.Sprintf("raw-ack-%d", run)
		_, err = sig.RecvPayload(ctx, ackTag)
		result.Duration = time.Since(start)
		if err != nil {
			logger.Warn("sender: receiver ack failed", "err", err)
		}

	} else {
		stream, ok := <-m.AcceptedStreams()
		if !ok {
			sessCancel()
			m.Close()
			for _, c := range cleanups {
				c()
			}
			mgr.CloseAll()
			return result, fmt.Errorf("no stream accepted")
		}

		buf := make([]byte, 32*1024)
		start := time.Now()
		var received atomic.Int64

		readDone := make(chan error, 1)
		go func() {
			var lastLog int64
			for {
				n, err := stream.Read(buf)
				if n > 0 {
					cur := received.Add(int64(n))
					if cur-lastLog >= 100*1024 {
						logger.Info("receiver: progress", "received", cur, "expected", totalBytes,
							"pct", fmt.Sprintf("%.1f%%", float64(cur)/float64(totalBytes)*100))
						lastLog = cur
					}
				}
				if received.Load() >= totalBytes {
					readDone <- nil
					return
				}
				if err != nil {
					readDone <- err
					return
				}
			}
		}()

		select {
		case err := <-readDone:
			if err != nil && err != io.EOF {
				logger.Warn("read error", "received", received.Load(), "expected", totalBytes, "err", err)
			}
		case <-time.After(60 * time.Second):
			logger.Warn("read timeout", "received", received.Load(), "expected", totalBytes)
		case <-ctx.Done():
			logger.Warn("read cancelled", "received", received.Load(), "expected", totalBytes)
		}

		result.Duration = time.Since(start)
		result.BytesSent = received.Load()

		// send ack to sender so it keeps connections alive until receiver is done
		ackTag := fmt.Sprintf("raw-ack-%d", run)
		_ = sig.SendPayload(ctx, ackTag, []byte("ok"))
		logger.Info("receiver: sent ack", "received", received.Load(), "expected", totalBytes)
	}

	result.Throughput = float64(result.BytesSent) * 8 / result.Duration.Seconds() / 1_000_000

	logger.Info("raw-dtls result",
		"run", run,
		"bytes", result.BytesSent,
		"duration", result.Duration.Round(time.Millisecond),
		"throughput_mbps", fmt.Sprintf("%.2f", result.Throughput),
	)

	// NOW safe to close everything — data has been confirmed received
	sessCancel()
	m.Close()
	for _, c := range cleanups {
		c()
	}
	mgr.CloseAll()

	return result, nil
}
