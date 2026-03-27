package dtls_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	internaldtls "github.com/call-vpn/call-vpn/internal/dtls"
	"github.com/call-vpn/call-vpn/internal/provider"
	"github.com/call-vpn/call-vpn/internal/testrig"
	"github.com/call-vpn/call-vpn/internal/turn"
)

// staticCreds implements provider.CredentialsProvider from static credentials.
type staticCreds struct{ c *provider.Credentials }

func (s *staticCreds) FetchCredentials(ctx context.Context) (*provider.Credentials, error) {
	return s.c, nil
}

// allocatePair creates two TURN allocations on the same server and returns them
// along with the resolved UDP addresses of each other's relay.
func allocatePair(t *testing.T, rig *testrig.TestRig) (
	clientAlloc, serverAlloc *turn.Allocation,
	serverRelayAddr, clientRelayAddr *net.UDPAddr,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	creds := rig.Credentials(0)

	clientMgr := turn.NewManager(&staticCreds{creds}, false, slog.Default())
	serverMgr := turn.NewManager(&staticCreds{creds}, false, slog.Default())

	clientAllocs, err := clientMgr.Allocate(ctx, 1)
	if err != nil {
		t.Fatalf("client allocate: %v", err)
	}
	t.Cleanup(func() { clientAllocs[0].Close() })

	serverAllocs, err := serverMgr.Allocate(ctx, 1)
	if err != nil {
		t.Fatalf("server allocate: %v", err)
	}
	t.Cleanup(func() { serverAllocs[0].Close() })

	serverRelayAddr, err = net.ResolveUDPAddr("udp", serverAllocs[0].RelayAddr.String())
	if err != nil {
		t.Fatalf("resolve server relay addr: %v", err)
	}
	clientRelayAddr, err = net.ResolveUDPAddr("udp", clientAllocs[0].RelayAddr.String())
	if err != nil {
		t.Fatalf("resolve client relay addr: %v", err)
	}

	return clientAllocs[0], serverAllocs[0], serverRelayAddr, clientRelayAddr
}

func TestDialAcceptOverTURN_SameServer(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})
	clientAlloc, serverAlloc, serverRelayAddr, clientRelayAddr := allocatePair(t, rig)

	// Punch both sides to create TURN permissions.
	if err := internaldtls.PunchRelay(clientAlloc.RelayConn, serverRelayAddr); err != nil {
		t.Fatalf("punch client->server: %v", err)
	}
	if err := internaldtls.PunchRelay(serverAlloc.RelayConn, clientRelayAddr); err != nil {
		t.Fatalf("punch server->client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start punch loops to keep permissions alive during handshake.
	punchCtx, punchCancel := context.WithCancel(ctx)
	defer punchCancel()
	go internaldtls.StartPunchLoop(punchCtx, clientAlloc.RelayConn, serverRelayAddr)
	go internaldtls.StartPunchLoop(punchCtx, serverAlloc.RelayConn, clientRelayAddr)

	type result struct {
		conn    net.Conn
		cleanup context.CancelFunc
		err     error
	}

	serverCh := make(chan result, 1)
	go func() {
		conn, cleanup, err := internaldtls.AcceptOverTURN(ctx, serverAlloc.RelayConn, clientRelayAddr)
		serverCh <- result{conn, cleanup, err}
	}()

	clientConn, clientCleanup, err := internaldtls.DialOverTURN(ctx, clientAlloc.RelayConn, serverRelayAddr, nil)
	if err != nil {
		t.Fatalf("DialOverTURN: %v", err)
	}
	defer clientCleanup()

	sr := <-serverCh
	if sr.err != nil {
		t.Fatalf("AcceptOverTURN: %v", sr.err)
	}
	defer sr.cleanup()

	punchCancel() // stop punch loops

	// Send "hello" from client to server.
	msg := []byte("hello")
	if _, err := clientConn.Write(msg); err != nil {
		t.Fatalf("client write: %v", err)
	}

	buf := make([]byte, 128)
	sr.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := sr.conn.Read(buf)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("got %q, want %q", buf[:n], "hello")
	}
}

func TestDialAcceptOverTURN_DataTransfer(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})
	clientAlloc, serverAlloc, serverRelayAddr, clientRelayAddr := allocatePair(t, rig)

	if err := internaldtls.PunchRelay(clientAlloc.RelayConn, serverRelayAddr); err != nil {
		t.Fatalf("punch client->server: %v", err)
	}
	if err := internaldtls.PunchRelay(serverAlloc.RelayConn, clientRelayAddr); err != nil {
		t.Fatalf("punch server->client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	punchCtx, punchCancel := context.WithCancel(ctx)
	defer punchCancel()
	go internaldtls.StartPunchLoop(punchCtx, clientAlloc.RelayConn, serverRelayAddr)
	go internaldtls.StartPunchLoop(punchCtx, serverAlloc.RelayConn, clientRelayAddr)

	type result struct {
		conn    net.Conn
		cleanup context.CancelFunc
		err     error
	}

	serverCh := make(chan result, 1)
	go func() {
		conn, cleanup, err := internaldtls.AcceptOverTURN(ctx, serverAlloc.RelayConn, clientRelayAddr)
		serverCh <- result{conn, cleanup, err}
	}()

	clientConn, clientCleanup, err := internaldtls.DialOverTURN(ctx, clientAlloc.RelayConn, serverRelayAddr, nil)
	if err != nil {
		t.Fatalf("DialOverTURN: %v", err)
	}
	defer clientCleanup()

	sr := <-serverCh
	if sr.err != nil {
		t.Fatalf("AcceptOverTURN: %v", sr.err)
	}
	defer sr.cleanup()

	punchCancel()

	// Generate 1MB of random data.
	const dataSize = 1 << 20
	data := make([]byte, dataSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generate random data: %v", err)
	}
	sentHash := sha256.Sum256(data)

	// Send from client in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		// Write in chunks to avoid exceeding DTLS record size.
		const chunkSize = 1024
		for off := 0; off < len(data); off += chunkSize {
			end := off + chunkSize
			if end > len(data) {
				end = len(data)
			}
			if _, err := clientConn.Write(data[off:end]); err != nil {
				errCh <- err
				return
			}
		}
		errCh <- nil
	}()

	// Receive on server side.
	received := make([]byte, 0, dataSize)
	buf := make([]byte, 2048)
	sr.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for len(received) < dataSize {
		n, err := sr.conn.Read(buf)
		if err != nil {
			t.Fatalf("server read after %d bytes: %v", len(received), err)
		}
		received = append(received, buf[:n]...)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("client write: %v", err)
	}

	recvHash := sha256.Sum256(received[:dataSize])
	if sentHash != recvHash {
		t.Fatalf("SHA-256 mismatch: sent %x, received %x", sentHash, recvHash)
	}
}

func TestDialAcceptOverTURN_PunchRequired(t *testing.T) {
	rig := testrig.New(t, testrig.Options{TURNServers: 1})

	// --- Without punch: should time out ---
	t.Run("without_punch", func(t *testing.T) {
		clientAlloc, serverAlloc, serverRelayAddr, clientRelayAddr := allocatePair(t, rig)

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		type result struct {
			conn    net.Conn
			cleanup context.CancelFunc
			err     error
		}

		serverCh := make(chan result, 1)
		go func() {
			conn, cleanup, err := internaldtls.AcceptOverTURN(ctx, serverAlloc.RelayConn, clientRelayAddr)
			serverCh <- result{conn, cleanup, err}
		}()

		_, clientCleanup, clientErr := internaldtls.DialOverTURN(ctx, clientAlloc.RelayConn, serverRelayAddr, nil)
		if clientErr == nil {
			clientCleanup()
			t.Fatal("expected DialOverTURN to fail without punch, but it succeeded")
		}
		t.Logf("DialOverTURN correctly failed without punch: %v", clientErr)

		sr := <-serverCh
		if sr.err == nil {
			sr.cleanup()
			t.Fatal("expected AcceptOverTURN to fail without punch, but it succeeded")
		}
		t.Logf("AcceptOverTURN correctly failed without punch: %v", sr.err)
	})

	// --- With punch: should succeed ---
	t.Run("with_punch", func(t *testing.T) {
		clientAlloc, serverAlloc, serverRelayAddr, clientRelayAddr := allocatePair(t, rig)

		if err := internaldtls.PunchRelay(clientAlloc.RelayConn, serverRelayAddr); err != nil {
			t.Fatalf("punch client->server: %v", err)
		}
		if err := internaldtls.PunchRelay(serverAlloc.RelayConn, clientRelayAddr); err != nil {
			t.Fatalf("punch server->client: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		punchCtx, punchCancel := context.WithCancel(ctx)
		defer punchCancel()
		go internaldtls.StartPunchLoop(punchCtx, clientAlloc.RelayConn, serverRelayAddr)
		go internaldtls.StartPunchLoop(punchCtx, serverAlloc.RelayConn, clientRelayAddr)

		type result struct {
			conn    net.Conn
			cleanup context.CancelFunc
			err     error
		}

		serverCh := make(chan result, 1)
		go func() {
			conn, cleanup, err := internaldtls.AcceptOverTURN(ctx, serverAlloc.RelayConn, clientRelayAddr)
			serverCh <- result{conn, cleanup, err}
		}()

		clientConn, clientCleanup, err := internaldtls.DialOverTURN(ctx, clientAlloc.RelayConn, serverRelayAddr, nil)
		if err != nil {
			t.Fatalf("DialOverTURN with punch: %v", err)
		}
		defer clientCleanup()

		sr := <-serverCh
		if sr.err != nil {
			t.Fatalf("AcceptOverTURN with punch: %v", sr.err)
		}
		defer sr.cleanup()

		punchCancel()

		// Verify data transfer works.
		msg := []byte("punch-test")
		if _, err := clientConn.Write(msg); err != nil {
			t.Fatalf("write: %v", err)
		}
		buf := make([]byte, 128)
		sr.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := sr.conn.Read(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(buf[:n]) != "punch-test" {
			t.Fatalf("got %q, want %q", buf[:n], "punch-test")
		}
	})
}

// Ensure io.Reader is usable (compile-time check used in DataTransfer test).
var _ = io.EOF
