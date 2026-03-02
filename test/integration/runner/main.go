package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	socks5Addr   = "client:1080"
	httpProxy    = "http://client:8080"
	targetURL    = "http://target/"
	expectedBody = "vpn-test-ok"
	pollTimeout  = 120 * time.Second
	pollInterval = 2 * time.Second
)

func main() {
	fmt.Println("=== Integration Test Runner ===")

	// Wait for client SOCKS5 proxy to become reachable
	fmt.Printf("waiting for SOCKS5 proxy at %s (timeout %s)...\n", socks5Addr, pollTimeout)
	if err := waitForPort(socks5Addr, pollTimeout); err != nil {
		fmt.Printf("FAIL: SOCKS5 proxy not reachable: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("SOCKS5 proxy is reachable")

	// Give proxies a moment to fully initialize
	time.Sleep(2 * time.Second)

	tests := []struct {
		name string
		fn   func() error
	}{
		{"SOCKS5 proxy → target (internal)", testSOCKS5},
		{"HTTP proxy → target (internal)", testHTTPProxy},
		{"SOCKS5 proxy → google.com (internet)", testSOCKS5Google},
		{"HTTP proxy → google.com (internet)", testHTTPProxyGoogle},
		{"SOCKS5 proxy → IP check (api.ipify.org)", testSOCKS5IP},
		{"HTTP proxy → IP check (api.ipify.org)", testHTTPProxyIP},
	}

	passed := 0
	for i, t := range tests {
		fmt.Printf("\n--- Test %d: %s ---\n", i+1, t.name)
		if err := t.fn(); err != nil {
			fmt.Printf("FAIL: %v\n", err)
		} else {
			fmt.Println("PASS")
			passed++
		}
	}

	fmt.Printf("\n=== Results: %d/%d passed ===\n", passed, len(tests))
	if passed == len(tests) {
		fmt.Println("ALL TESTS PASSED")
		os.Exit(0)
	}
	fmt.Println("SOME TESTS FAILED")
	os.Exit(1)
}

func waitForPort(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

// socks5Dial performs a SOCKS5 handshake and connects to the given domain:port.
// Returns the established connection ready for data transfer.
func socks5Dial(domain string, port uint16) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", socks5Addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial SOCKS5: %w", err)
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	// Client greeting: VER=5, NMETHODS=1, METHOD=0 (no auth)
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 greeting: %w", err)
	}

	// Server response: VER=5, METHOD=0
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 greeting response: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 unexpected greeting: %x", resp)
	}

	// Connect request: VER=5, CMD=1, RSV=0, ATYP=3 (domain)
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(domain))}
	req = append(req, []byte(domain)...)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, port)
	req = append(req, portBytes...)

	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect: %w", err)
	}

	// Connect response: VER=5, REP, RSV, ATYP, BND.ADDR, BND.PORT
	connResp := make([]byte, 4)
	if _, err := io.ReadFull(conn, connResp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect response: %w", err)
	}
	if connResp[1] != 0x00 {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect failed, rep=%d", connResp[1])
	}

	// Skip bound address bytes based on ATYP
	switch connResp[3] {
	case 0x01: // IPv4
		skip := make([]byte, 4+2)
		io.ReadFull(conn, skip)
	case 0x03: // Domain
		lenBuf := make([]byte, 1)
		io.ReadFull(conn, lenBuf)
		skip := make([]byte, int(lenBuf[0])+2)
		io.ReadFull(conn, skip)
	case 0x04: // IPv6
		skip := make([]byte, 16+2)
		io.ReadFull(conn, skip)
	}

	return conn, nil
}

// testSOCKS5 connects through the SOCKS5 proxy to the internal target HTTP server.
func testSOCKS5() error {
	conn, err := socks5Dial("target", 80)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Send HTTP request through the tunnel
	httpReq := "GET / HTTP/1.1\r\nHost: target\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		return fmt.Errorf("http request: %w", err)
	}

	httpResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	got := string(body)
	if got != expectedBody {
		return fmt.Errorf("expected %q, got %q", expectedBody, got)
	}
	return nil
}

// testHTTPProxy tests the HTTP proxy with the internal target.
func testHTTPProxy() error {
	return httpProxyGet(targetURL, func(resp *http.Response, body []byte) error {
		if resp.StatusCode != 200 {
			return fmt.Errorf("unexpected status %d, body: %s", resp.StatusCode, string(body))
		}
		if string(body) != expectedBody {
			return fmt.Errorf("expected %q, got %q", expectedBody, string(body))
		}
		return nil
	})
}

// testSOCKS5Google connects to google.com:80 through the SOCKS5 proxy.
// Expects a valid HTTP response (200 or 301 redirect to HTTPS).
func testSOCKS5Google() error {
	conn, err := socks5Dial("google.com", 80)
	if err != nil {
		return err
	}
	defer conn.Close()

	httpReq := "GET / HTTP/1.1\r\nHost: google.com\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		return fmt.Errorf("http request: %w", err)
	}

	httpResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	defer httpResp.Body.Close()
	io.Copy(io.Discard, httpResp.Body)

	return validateGoogleResponse(httpResp)
}

// testHTTPProxyGoogle sends a request to google.com through the HTTP proxy.
func testHTTPProxyGoogle() error {
	return httpProxyGet("http://google.com/", func(resp *http.Response, body []byte) error {
		return validateGoogleResponse(resp)
	})
}

// validateGoogleResponse checks that we got a valid HTTP response from Google.
// google.com:80 typically returns 200 or 301 redirect to HTTPS.
func validateGoogleResponse(resp *http.Response) error {
	fmt.Printf("  google.com status: %d\n", resp.StatusCode)

	switch resp.StatusCode {
	case 200:
		// Direct 200 OK
		return nil
	case 301, 302:
		// Redirect — verify Location header points to Google
		loc := resp.Header.Get("Location")
		fmt.Printf("  redirect to: %s\n", loc)
		if loc == "" {
			return fmt.Errorf("301/302 but no Location header")
		}
		if !strings.Contains(loc, "google") {
			return fmt.Errorf("unexpected redirect location: %s", loc)
		}
		return nil
	default:
		return fmt.Errorf("unexpected status %d from google.com", resp.StatusCode)
	}
}

// testSOCKS5IP checks the tunnel's exit IP via api.ipify.org through SOCKS5.
func testSOCKS5IP() error {
	conn, err := socks5Dial("api.ipify.org", 80)
	if err != nil {
		return err
	}
	defer conn.Close()

	httpReq := "GET / HTTP/1.1\r\nHost: api.ipify.org\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		return fmt.Errorf("http request: %w", err)
	}

	httpResp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	return validateIPResponse(httpResp.StatusCode, string(body))
}

// testHTTPProxyIP checks the tunnel's exit IP via api.ipify.org through HTTP proxy.
func testHTTPProxyIP() error {
	return httpProxyGet("http://api.ipify.org/", func(resp *http.Response, body []byte) error {
		return validateIPResponse(resp.StatusCode, string(body))
	})
}

// validateIPResponse verifies that the response is a valid IP address.
func validateIPResponse(status int, body string) error {
	if status != 200 {
		return fmt.Errorf("unexpected status %d, body: %s", status, body)
	}

	ip := strings.TrimSpace(body)
	fmt.Printf("  exit IP: %s\n", ip)

	if net.ParseIP(ip) == nil {
		return fmt.Errorf("response is not a valid IP address: %q", ip)
	}
	return nil
}

// httpProxyGet sends an HTTP GET through the HTTP proxy and calls validate on the response.
func httpProxyGet(targetURL string, validate func(*http.Response, []byte) error) error {
	proxyURL, _ := url.Parse(httpProxy)
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Don't follow redirects — we want to inspect the raw response
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(targetURL)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	return validate(resp, body)
}
