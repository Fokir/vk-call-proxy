package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
)

// session groups multiple DTLS connections from a single client.
type session struct {
	mu     sync.Mutex
	m      *mux.Mux
	logger *slog.Logger
	cancel context.CancelFunc
	conns  int
}

var sessions sync.Map // [16]byte -> *session

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:9000", "DTLS UDP listen address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	siren := monitoring.New(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutting down server")
		cancel()
	}()

	ln, err := internaldtls.Listen(*listenAddr)
	if err != nil {
		logger.Error("failed to start DTLS listener", "err", err)
		os.Exit(1)
	}
	defer ln.Close()

	logger.Info("server listening (DTLS/UDP)", "addr", *listenAddr)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("accept error", "err", err)
			continue
		}
		go handleConnection(ctx, logger, siren, conn)
	}
}

func handleConnection(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren, conn net.Conn) {
	// Read session ID (first 16 bytes after DTLS handshake).
	sessionID, err := mux.ReadSessionID(conn)
	if err != nil {
		logger.Warn("read session id failed", "remote", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}

	logger.Info("connection received",
		"remote", conn.RemoteAddr(),
		"session_id", fmt.Sprintf("%x", sessionID),
	)

	sess := getOrCreateSession(ctx, logger, siren, sessionID)

	sess.mu.Lock()
	sess.m.AddConn(conn)
	sess.conns++
	count := sess.conns
	sess.mu.Unlock()

	logger.Info("connection added to session",
		"session_id", fmt.Sprintf("%x", sessionID),
		"total_conns", count,
	)
}

func getOrCreateSession(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren, id [16]byte) *session {
	val, loaded := sessions.LoadOrStore(id, (*session)(nil))
	if loaded && val != nil {
		return val.(*session)
	}

	// First connection for this session — create it.
	sessCtx, sessCancel := context.WithCancel(ctx)
	sessLogger := logger.With("session_id", fmt.Sprintf("%x", id))
	m := mux.New(sessLogger) // Zero initial connections; AddConn later.

	sess := &session{
		m:      m,
		logger: sessLogger,
		cancel: sessCancel,
	}
	sessions.Store(id, sess)

	// Start AcceptStream loop.
	go func() {
		defer func() {
			sessions.Delete(id)
			m.Close()
			sessCancel()
			sessLogger.Info("session closed")
		}()

		go m.DispatchLoop(sessCtx)

		for {
			stream, err := m.AcceptStream(sessCtx)
			if err != nil {
				if sessCtx.Err() != nil {
					return
				}
				sessLogger.Warn("accept stream error", "err", err)
				siren.AlertDisconnect(sessCtx, fmt.Sprintf("session-%x", id))
				return
			}
			go handleStream(sessCtx, sessLogger, stream)
		}
	}()

	// Cleanup timer: if session gets no activity, close after timeout.
	go func() {
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		select {
		case <-timer.C:
			// Session timeout — only if no streams were handled
		case <-sessCtx.Done():
		}
	}()

	return sess
}

func handleStream(ctx context.Context, logger *slog.Logger, stream *mux.Stream) {
	defer stream.Close()

	addrBuf := make([]byte, 512)
	n, err := stream.Read(addrBuf)
	if err != nil {
		logger.Debug("read target address failed", "err", err)
		return
	}
	target := string(addrBuf[:n])

	logger.Debug("connecting to target", "stream_id", stream.ID, "target", target)

	dialer := net.Dialer{}
	outConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		logger.Warn("dial target failed", "target", target, "err", err)
		return
	}
	defer outConn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(outConn, stream)
	}()

	go func() {
		defer wg.Done()
		io.Copy(stream, outConn)
	}()

	wg.Wait()
}
