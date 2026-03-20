package http

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
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

func TestHTTPConnectEcho(t *testing.T) {
	// 1. Start echo server
	echo := startEchoServer(t)
	defer echo.Close()
	echoAddr := echo.Addr().String()

	// 2. Start HTTP proxy
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

	// Wait briefly for the server to start
	time.Sleep(50 * time.Millisecond)

	// 3. Connect to proxy and send CONNECT request
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Send CONNECT request
	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	if _, err := conn.Write([]byte(connectReq)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// Read response
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT status: got %d, want 200", resp.StatusCode)
	}

	// 4. Send data through the tunnel, verify echo
	payload := []byte("hello through http connect tunnel")
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

func TestHTTPConnectDialError(t *testing.T) {
	// Test that dial failure returns 502 Bad Gateway
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
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	connectReq := fmt.Sprintf("CONNECT 192.0.2.1:80 HTTP/1.1\r\nHost: 192.0.2.1:80\r\n\r\n")
	conn.Write([]byte(connectReq))

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}

	cancel()
}

func TestHTTPProxyPlainRequest(t *testing.T) {
	// Start a simple HTTP server (not echo — proper HTTP responses)
	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer httpLn.Close()

	httpSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Test", "proxied")
			w.WriteHeader(200)
			fmt.Fprint(w, "OK from backend")
		}),
	}
	go httpSrv.Serve(httpLn)
	defer httpSrv.Close()

	backendAddr := httpLn.Addr().String()

	// Start HTTP proxy
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

	go srv.ListenAndServe(ctx)
	time.Sleep(50 * time.Millisecond)

	// Send a plain HTTP request through the proxy
	proxyURL, _ := url.Parse(fmt.Sprintf("http://%s", proxyAddr))
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}

	targetURL := fmt.Sprintf("http://%s/test", backendAddr)
	req, _ := http.NewRequest("GET", targetURL, nil)

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "OK from backend") {
		t.Fatalf("unexpected body: %s", body)
	}

	if resp.Header.Get("X-Test") != "proxied" {
		t.Fatalf("missing X-Test header")
	}

	cancel()
}
