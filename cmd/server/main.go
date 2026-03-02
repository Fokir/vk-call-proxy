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
	"sync/atomic"
	"syscall"

	"github.com/call-vpn/call-vpn/internal/monitoring"
	"github.com/call-vpn/call-vpn/internal/mux"
)

func main() {
	listenAddr := flag.String("listen", "0.0.0.0:9000", "Address to listen for mux client connections")
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

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		logger.Error("failed to listen", "err", err)
		os.Exit(1)
	}
	defer ln.Close()

	logger.Info("server listening", "addr", *listenAddr)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	var clientID atomic.Uint64

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Warn("accept error", "err", err)
			continue
		}
		id := clientID.Add(1)
		logger.Info("client connected", "client_id", id, "remote", conn.RemoteAddr())
		go handleClient(ctx, logger, siren, conn, id)
	}
}

func handleClient(ctx context.Context, logger *slog.Logger, siren *monitoring.Siren, conn net.Conn, clientID uint64) {
	defer conn.Close()

	clientLogger := logger.With("client_id", clientID)

	// The client sends mux frames over a single TCP connection.
	// Each mux stream maps to one outbound connection to the real internet.
	m := mux.New(clientLogger, conn)
	defer m.Close()

	for {
		stream, err := m.AcceptStream(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			clientLogger.Warn("accept stream error", "err", err)
			siren.AlertDisconnect(ctx, fmt.Sprintf("client-%d", clientID))
			return
		}
		go handleStream(ctx, clientLogger, stream)
	}
}

func handleStream(ctx context.Context, logger *slog.Logger, stream *mux.Stream) {
	defer stream.Close()

	// Read the first frame which contains the target address (host:port).
	addrBuf := make([]byte, 512)
	n, err := stream.Read(addrBuf)
	if err != nil {
		logger.Debug("read target address failed", "err", err)
		return
	}
	target := string(addrBuf[:n])

	logger.Debug("connecting to target", "stream_id", stream.ID, "target", target)

	// Dial the actual target on the internet.
	dialer := net.Dialer{}
	outConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		logger.Warn("dial target failed", "target", target, "err", err)
		return
	}
	defer outConn.Close()

	// Bidirectional relay: stream <-> target.
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
