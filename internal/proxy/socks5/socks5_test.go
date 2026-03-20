package socks5

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// startEchoServer starts a TCP server that echoes everything back.
func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	return ln
}

// freeAddr returns a free TCP address by binding to :0 and releasing it.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestSOCKS5Echo(t *testing.T) {
	// 1. Start echo server
	echo := startEchoServer(t)
	defer echo.Close()
	echoAddr := echo.Addr().String()
	echoHost, echoPortStr, _ := net.SplitHostPort(echoAddr)

	// 2. Start SOCKS5 server
	proxyAddr := freeAddr(t)
	srv := &Server{
		Addr: proxyAddr,
		Dial: func(ctx context.Context, network, addr string) (io.ReadWriteCloser, error) {
			return net.DialTimeout("tcp", addr, 5*time.Second)
		},
		Logger: slog.Default(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe(ctx)
	}()

	// Wait briefly for the server to start accepting
	time.Sleep(50 * time.Millisecond)

	// 3. Connect to SOCKS5 proxy
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial socks5: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// 4. SOCKS5 handshake: version 5, 1 method, no auth (0x00)
	_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	if err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	// Read server response: VER | METHOD
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("read handshake resp: %v", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("unexpected handshake response: %x", resp)
	}

	// 5. CONNECT request to echo server (IPv4)
	echoIP := net.ParseIP(echoHost).To4()
	if echoIP == nil {
		t.Fatal("echo server not on IPv4")
	}

	var echoPort uint16
	for _, c := range echoPortStr {
		echoPort = echoPort*10 + uint16(c-'0')
	}

	req := make([]byte, 10)
	req[0] = 0x05 // VER
	req[1] = 0x01 // CMD: CONNECT
	req[2] = 0x00 // RSV
	req[3] = 0x01 // ATYP: IPv4
	copy(req[4:8], echoIP)
	binary.BigEndian.PutUint16(req[8:10], echoPort)

	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write connect request: %v", err)
	}

	// Read CONNECT response: VER | REP | RSV | ATYP | BND.ADDR(4) | BND.PORT(2)
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read connect reply: %v", err)
	}
	if reply[1] != 0x00 {
		t.Fatalf("CONNECT failed with reply code: 0x%02x", reply[1])
	}

	// 6. Send data through the tunnel, verify echo
	payload := []byte("hello through socks5 tunnel")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echoed) != string(payload) {
		t.Fatalf("echo mismatch: got %q, want %q", echoed, payload)
	}

	// Cleanup
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}

func TestSOCKS5DomainConnect(t *testing.T) {
	// Test CONNECT with domain name address type (ATYP 0x03)
	echo := startEchoServer(t)
	defer echo.Close()
	echoAddr := echo.Addr().String()

	proxyAddr := freeAddr(t)
	srv := &Server{
		Addr: proxyAddr,
		Dial: func(ctx context.Context, network, addr string) (io.ReadWriteCloser, error) {
			// Resolve "localhost" to the actual echo server address
			host, port, _ := net.SplitHostPort(addr)
			if host == "localhost" {
				_, echoPort, _ := net.SplitHostPort(echoAddr)
				addr = net.JoinHostPort("127.0.0.1", echoPort)
				_ = port
			}
			return net.DialTimeout("tcp", addr, 5*time.Second)
		},
		Logger: slog.Default(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.ListenAndServe(ctx)
	time.Sleep(50 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial socks5: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Handshake
	conn.Write([]byte{0x05, 0x01, 0x00})
	resp := make([]byte, 2)
	io.ReadFull(conn, resp)

	// CONNECT with domain name
	_, echoPortStr, _ := net.SplitHostPort(echoAddr)
	var echoPort uint16
	for _, c := range echoPortStr {
		echoPort = echoPort*10 + uint16(c-'0')
	}

	domain := "localhost"
	req := make([]byte, 0, 7+len(domain))
	req = append(req, 0x05, 0x01, 0x00, 0x03) // VER, CMD, RSV, ATYP=domain
	req = append(req, byte(len(domain)))
	req = append(req, []byte(domain)...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, echoPort)
	req = append(req, portBuf...)

	conn.Write(req)

	reply := make([]byte, 10)
	io.ReadFull(conn, reply)
	if reply[1] != 0x00 {
		t.Fatalf("domain CONNECT failed: 0x%02x", reply[1])
	}

	payload := []byte("domain test data")
	conn.Write(payload)

	echoed := make([]byte, len(payload))
	io.ReadFull(conn, echoed)
	if string(echoed) != string(payload) {
		t.Fatalf("echo mismatch: got %q, want %q", echoed, payload)
	}

	cancel()
}

func TestSOCKS5DialError(t *testing.T) {
	// Test that a dial failure returns proper SOCKS5 error reply
	proxyAddr := freeAddr(t)
	srv := &Server{
		Addr: proxyAddr,
		Dial: func(ctx context.Context, network, addr string) (io.ReadWriteCloser, error) {
			return nil, net.UnknownNetworkError("simulated failure")
		},
		Logger: slog.Default(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.ListenAndServe(ctx)
	time.Sleep(50 * time.Millisecond)

	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial socks5: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Handshake
	conn.Write([]byte{0x05, 0x01, 0x00})
	resp := make([]byte, 2)
	io.ReadFull(conn, resp)

	// CONNECT to unreachable target
	req := []byte{
		0x05, 0x01, 0x00, 0x01,
		192, 0, 2, 1, // 192.0.2.1 (TEST-NET)
		0x00, 0x50, // port 80
	}
	conn.Write(req)

	reply := make([]byte, 10)
	io.ReadFull(conn, reply)
	if reply[1] == 0x00 {
		t.Fatal("expected non-zero reply code for dial failure")
	}

	cancel()
}
