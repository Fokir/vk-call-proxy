package mux_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/mux"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/testrig"
	"github.com/call-vpn/call-vpn/internal/turn"
)

// staticCreds implements provider.CredentialsProvider from static credentials.
type staticCreds struct{ c *provider.Credentials }

func (s *staticCreds) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	return s.c, nil
}

// dtlsPair holds a DTLS client/server connection pair and cleanup resources.
type dtlsPair struct {
	clientConn net.Conn
	serverConn net.Conn
	cleanup    func()
}

// setupDTLSPair creates two DTLS connections through local TURN and returns them.
func setupDTLSPair(t *testing.T, rig *testrig.TestRig) dtlsPair {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)

	creds := rig.Credentials(0)

	clientMgr := turn.NewManager(&staticCreds{creds}, false, slog.Default())
	serverMgr := turn.NewManager(&staticCreds{creds}, false, slog.Default())

	clientAllocs, err := clientMgr.Allocate(ctx, 1)
	if err != nil {
		cancel()
		t.Fatalf("client allocate: %v", err)
	}

	serverAllocs, err := serverMgr.Allocate(ctx, 1)
	if err != nil {
		cancel()
		t.Fatalf("server allocate: %v", err)
	}

	serverRelayAddr, err := net.ResolveUDPAddr("udp", serverAllocs[0].RelayAddr.String())
	if err != nil {
		cancel()
		t.Fatalf("resolve server relay addr: %v", err)
	}
	clientRelayAddr, err := net.ResolveUDPAddr("udp", clientAllocs[0].RelayAddr.String())
	if err != nil {
		cancel()
		t.Fatalf("resolve client relay addr: %v", err)
	}

	// Punch both sides.
	if err := internaldtls.PunchRelay(clientAllocs[0].RelayConn, serverRelayAddr); err != nil {
		cancel()
		t.Fatalf("punch client->server: %v", err)
	}
	if err := internaldtls.PunchRelay(serverAllocs[0].RelayConn, clientRelayAddr); err != nil {
		cancel()
		t.Fatalf("punch server->client: %v", err)
	}

	// Start punch loops.
	punchCtx, punchCancel := context.WithCancel(ctx)
	go internaldtls.StartPunchLoop(punchCtx, clientAllocs[0].RelayConn, serverRelayAddr)
	go internaldtls.StartPunchLoop(punchCtx, serverAllocs[0].RelayConn, clientRelayAddr)

	type result struct {
		conn    net.Conn
		cleanup context.CancelFunc
		err     error
	}

	serverCh := make(chan result, 1)
	go func() {
		conn, cleanup, err := internaldtls.AcceptOverTURN(ctx, serverAllocs[0].RelayConn, clientRelayAddr)
		serverCh <- result{conn, cleanup, err}
	}()

	clientConn, clientCleanup, err := internaldtls.DialOverTURN(ctx, clientAllocs[0].RelayConn, serverRelayAddr, nil)
	if err != nil {
		punchCancel()
		cancel()
		t.Fatalf("DialOverTURN: %v", err)
	}

	sr := <-serverCh
	if sr.err != nil {
		clientCleanup()
		punchCancel()
		cancel()
		t.Fatalf("AcceptOverTURN: %v", sr.err)
	}

	punchCancel()

	return dtlsPair{
		clientConn: clientConn,
		serverConn: sr.conn,
		cleanup: func() {
			clientCleanup()
			sr.cleanup()
			clientAllocs[0].Close()
			serverAllocs[0].Close()
			cancel()
		},
	}
}

func TestMUX_E2E_StreamOverTURN(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})
	pair := setupDTLSPair(t, rig)
	t.Cleanup(pair.cleanup)

	logger := slog.Default()

	clientMux := mux.New(logger, pair.clientConn)
	serverMux := mux.New(logger, pair.serverConn)
	serverMux.EnableStreamAccept(8)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go serverMux.DispatchLoop(ctx)
	time.Sleep(50 * time.Millisecond)

	// Client opens stream and writes data.
	s, err := clientMux.OpenStream(1)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	msg := []byte("hello world")
	if _, err := s.Write(msg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Server accepts stream and reads data.
	var rs *mux.Stream
	select {
	case rs = <-serverMux.AcceptedStreams():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for accepted stream")
	}

	buf := make([]byte, 128)
	n, err := rs.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "hello world" {
		t.Fatalf("got %q, want %q", buf[:n], "hello world")
	}

	cancel()
	clientMux.Close()
	serverMux.Close()
}

func TestMUX_E2E_MultiStream(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})
	pair := setupDTLSPair(t, rig)
	t.Cleanup(pair.cleanup)

	logger := slog.Default()

	clientMux := mux.New(logger, pair.clientConn)
	serverMux := mux.New(logger, pair.serverConn)
	serverMux.EnableStreamAccept(64)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go serverMux.DispatchLoop(ctx)
	time.Sleep(50 * time.Millisecond)

	const numStreams = 10
	const dataSize = 10 * 1024

	// Generate unique payloads per stream.
	payloads := make([][]byte, numStreams)
	hashes := make([][32]byte, numStreams)
	for i := range payloads {
		payloads[i] = make([]byte, dataSize)
		if _, err := rand.Read(payloads[i]); err != nil {
			t.Fatalf("generate payload %d: %v", i, err)
		}
		hashes[i] = sha256.Sum256(payloads[i])
	}

	// Collect accepted streams on server side.
	acceptedMap := &sync.Map{}
	go func() {
		for s := range serverMux.AcceptedStreams() {
			acceptedMap.Store(s.ID, s)
		}
	}()

	// Write from client in parallel goroutines.
	var wg sync.WaitGroup
	for i := 0; i < numStreams; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s, err := clientMux.OpenStream(uint32(idx + 1))
			if err != nil {
				t.Errorf("OpenStream(%d): %v", idx+1, err)
				return
			}
			// Write in chunks to avoid exceeding DTLS record limits.
			const chunkSize = 1024
			for off := 0; off < len(payloads[idx]); off += chunkSize {
				end := off + chunkSize
				if end > len(payloads[idx]) {
					end = len(payloads[idx])
				}
				if _, err := s.Write(payloads[idx][off:end]); err != nil {
					t.Errorf("stream %d write: %v", idx+1, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// Read all streams on server side.
	time.Sleep(500 * time.Millisecond) // let dispatch deliver all frames

	for i := 0; i < numStreams; i++ {
		streamID := uint32(i + 1)
		val, ok := acceptedMap.Load(streamID)
		if !ok {
			t.Fatalf("stream %d not accepted", streamID)
		}
		rs := val.(*mux.Stream)

		var received []byte
		buf := make([]byte, 4096)
		// Use a deadline-based approach to read all available data.
		for len(received) < dataSize {
			n, err := rs.Read(buf)
			if n > 0 {
				received = append(received, buf[:n]...)
			}
			if err != nil {
				break
			}
		}

		if len(received) < dataSize {
			t.Fatalf("stream %d: received %d bytes, want %d", streamID, len(received), dataSize)
		}

		recvHash := sha256.Sum256(received[:dataSize])
		if recvHash != hashes[i] {
			t.Fatalf("stream %d: SHA-256 mismatch", streamID)
		}
	}

	cancel()
	clientMux.Close()
	serverMux.Close()
}

func TestMUX_E2E_AddConn(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})

	// Create 3 DTLS pairs.
	pairs := make([]dtlsPair, 3)
	for i := range pairs {
		pairs[i] = setupDTLSPair(t, rig)
		t.Cleanup(pairs[i].cleanup)
	}

	logger := slog.Default()

	// Start MUX with first connection only.
	clientMux := mux.New(logger, pairs[0].clientConn)
	serverMux := mux.New(logger, pairs[0].serverConn)

	// Add 2 more connections.
	for i := 1; i < 3; i++ {
		clientMux.AddConn(pairs[i].clientConn)
		serverMux.AddConn(pairs[i].serverConn)
	}

	if got := clientMux.ActiveConns(); got != 3 {
		t.Fatalf("client ActiveConns = %d, want 3", got)
	}
	if got := serverMux.ActiveConns(); got != 3 {
		t.Fatalf("server ActiveConns = %d, want 3", got)
	}

	// Verify data transfer works through the multi-conn MUX.
	serverMux.EnableStreamAccept(8)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go serverMux.DispatchLoop(ctx)
	time.Sleep(50 * time.Millisecond)

	s, err := clientMux.OpenStream(1)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	msg := []byte("multi-conn-test")
	if _, err := s.Write(msg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var rs *mux.Stream
	select {
	case rs = <-serverMux.AcceptedStreams():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for accepted stream")
	}

	buf := make([]byte, 128)
	n, err := rs.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "multi-conn-test" {
		t.Fatalf("got %q, want %q", buf[:n], "multi-conn-test")
	}

	cancel()
	clientMux.Close()
	serverMux.Close()
}

func TestMUX_E2E_ConnDied(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})

	// Create 2 DTLS pairs.
	pairs := make([]dtlsPair, 2)
	for i := range pairs {
		pairs[i] = setupDTLSPair(t, rig)
		t.Cleanup(pairs[i].cleanup)
	}

	logger := slog.Default()

	clientMux := mux.New(logger, pairs[0].clientConn, pairs[1].clientConn)
	serverMux := mux.New(logger, pairs[0].serverConn, pairs[1].serverConn)
	serverMux.EnableStreamAccept(8)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	go serverMux.DispatchLoop(ctx)
	go clientMux.DispatchLoop(ctx)
	time.Sleep(50 * time.Millisecond)

	if got := clientMux.ActiveConns(); got != 2 {
		t.Fatalf("client ActiveConns = %d, want 2", got)
	}

	// Kill the first connection by closing the underlying DTLS conn.
	pairs[0].clientConn.Close()

	// ConnDied should fire on the client side.
	select {
	case idx := <-clientMux.ConnDied():
		t.Logf("ConnDied fired for conn index %d", idx)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for ConnDied")
	}

	// Verify the surviving connection still works.
	// Give MUX time to process the dead conn.
	time.Sleep(200 * time.Millisecond)

	s, err := clientMux.OpenStream(42)
	if err != nil {
		t.Fatalf("OpenStream after conn death: %v", err)
	}

	msg := []byte(fmt.Sprintf("surviving-conn-%d", time.Now().UnixNano()))
	if _, err := s.Write(msg); err != nil {
		t.Fatalf("Write after conn death: %v", err)
	}

	var rs *mux.Stream
	select {
	case rs = <-serverMux.AcceptedStreams():
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for accepted stream after conn death")
	}

	buf := make([]byte, 256)
	n, err := rs.Read(buf)
	if err != nil {
		t.Fatalf("Read after conn death: %v", err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Fatalf("got %q, want %q", buf[:n], msg)
	}

	cancel()
	clientMux.Close()
	serverMux.Close()
}

func TestMUX_E2E_RawPackets(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})
	pair := setupDTLSPair(t, rig)
	t.Cleanup(pair.cleanup)

	logger := slog.Default()

	clientMux := mux.New(logger, pair.clientConn)
	serverMux := mux.New(logger, pair.serverConn)

	// Enable raw packets on the receiver (server) side.
	serverMux.EnableRawPackets(64)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go serverMux.DispatchLoop(ctx)
	time.Sleep(50 * time.Millisecond)

	// Send a raw packet (StreamID=0, FrameData) from client.
	payload := []byte("raw-ip-packet-test-data")
	f := &mux.Frame{
		StreamID: 0,
		Type:     mux.FrameData,
		Payload:  payload,
	}
	f.Length = uint32(len(payload))
	if err := clientMux.SendRawPacket(f); err != nil {
		t.Fatalf("SendRawPacket: %v", err)
	}

	// Read from the RawPackets ring buffer on server side.
	rb := serverMux.RawPackets()
	if rb == nil {
		t.Fatal("RawPackets() returned nil")
	}

	var received *mux.Frame
	select {
	case <-rb.Ready():
		var ok bool
		received, ok = rb.Pop()
		if !ok {
			t.Fatal("Pop returned false after Ready signal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for raw packet")
	}

	if !bytes.Equal(received.Payload, payload) {
		t.Fatalf("raw packet data mismatch: got %q, want %q", received.Payload, payload)
	}

	cancel()
	clientMux.Close()
	serverMux.Close()
}

func TestMUX_E2E_MultiConnBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-conn benchmark in short mode")
	}

	rig := testrig.New(t, testrig.Options{TURNServers: 1})

	connCounts := []int{1, 2}
	const payloadSize = 256 * 1024 // 256 KB

	for _, numConns := range connCounts {
		t.Run(fmt.Sprintf("conns=%d", numConns), func(t *testing.T) {
			// Create N DTLS pairs.
			pairs := make([]dtlsPair, numConns)
			for i := range pairs {
				pairs[i] = setupDTLSPair(t, rig)
				t.Cleanup(pairs[i].cleanup)
			}

			logger := slog.Default()

			// Build client/server MUX with first connection.
			clientMux := mux.New(logger, pairs[0].clientConn)
			serverMux := mux.New(logger, pairs[0].serverConn)
			for i := 1; i < numConns; i++ {
				clientMux.AddConn(pairs[i].clientConn)
				serverMux.AddConn(pairs[i].serverConn)
			}

			// No explicit striping — use default adaptive mode (off for short transfers).
			serverMux.EnableStreamAccept(16)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			go serverMux.DispatchLoop(ctx)
			time.Sleep(50 * time.Millisecond)

			// Generate payload.
			payload := make([]byte, payloadSize)
			if _, err := rand.Read(payload); err != nil {
				t.Fatalf("rand.Read: %v", err)
			}

			// Open stream on client.
			stream, err := clientMux.OpenStream(1)
			if err != nil {
				t.Fatalf("OpenStream: %v", err)
			}

			// Receive in background.
			var recvWg sync.WaitGroup
			recvWg.Add(1)
			var recvErr error
			var recvBytes int
			go func() {
				defer recvWg.Done()
				select {
				case srv := <-serverMux.AcceptedStreams():
					buf := make([]byte, payloadSize)
					total := 0
					for total < payloadSize {
						n, err := srv.Read(buf[total:])
						if n > 0 {
							total += n
						}
						if err != nil {
							if total >= payloadSize {
								break
							}
							recvErr = err
							recvBytes = total
							return
						}
					}
					recvBytes = total
				case <-ctx.Done():
					recvErr = ctx.Err()
				}
			}()

			// Send payload.
			start := time.Now()
			written := 0
			for written < payloadSize {
				n, err := stream.Write(payload[written:])
				if err != nil {
					t.Fatalf("write at offset %d: %v", written, err)
				}
				written += n
			}

			recvWg.Wait()
			elapsed := time.Since(start)

			if recvErr != nil {
				t.Fatalf("receive error: %v (got %d bytes)", recvErr, recvBytes)
			}
			if recvBytes != payloadSize {
				t.Fatalf("received %d bytes, want %d", recvBytes, payloadSize)
			}

			mbps := float64(payloadSize) / elapsed.Seconds() / (1 << 20)
			t.Logf("conns=%d throughput=%.2f MB/s (%d bytes in %v)",
				numConns, mbps, payloadSize, elapsed.Round(time.Millisecond))

			cancel()
			clientMux.Close()
			serverMux.Close()
		})
	}
}
