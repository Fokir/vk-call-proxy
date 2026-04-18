// Package upstream provides a dialer that routes TCP connections through
// an upstream proxy (SOCKS5 or HTTP CONNECT).
package upstream

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"golang.org/x/net/proxy"
)

// DialContextFunc is a function that dials a network address with context.
type DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ParseProxyURL parses a proxy URL and returns a DialContext function.
// Supported schemes: socks5://, http://. Note: https:// (HTTP CONNECT over TLS) is not supported.
// Returns nil, nil if rawURL is empty.
func ParseProxyURL(rawURL string) (DialContextFunc, error) {
	if rawURL == "" {
		return nil, nil
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}

	switch u.Scheme {
	case "socks5":
		return socks5Dialer(u)
	case "http":
		return httpConnectDialer(u), nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %q (use socks5:// or http://)", u.Scheme)
	}
}

func socks5Dialer(u *url.URL) (DialContextFunc, error) {
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}
	d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
	}
	// proxy.SOCKS5 returns a proxy.Dialer; check if it supports DialContext.
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext, nil
	}
	// Fallback: wrap Dial without context support.
	return func(_ context.Context, network, addr string) (net.Conn, error) {
		return d.Dial(network, addr)
	}, nil
}

func httpConnectDialer(u *url.URL) DialContextFunc {
	proxyAddr := u.Host
	var proxyAuth string
	if u.User != nil {
		pass, _ := u.User.Password()
		cred := u.User.Username() + ":" + pass
		proxyAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(cred))
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Connect to the proxy server.
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("connect to HTTP proxy %s: %w", proxyAddr, err)
		}

		// Send CONNECT request.
		req := &http.Request{
			Method: http.MethodConnect,
			URL:    &url.URL{Opaque: addr},
			Host:   addr,
			Header: make(http.Header),
		}
		if proxyAuth != "" {
			req.Header.Set("Proxy-Authorization", proxyAuth)
		}
		if err := req.Write(conn); err != nil {
			conn.Close()
			return nil, fmt.Errorf("write CONNECT request: %w", err)
		}

		// Read response. Use size-1 buffer to avoid consuming tunnel data.
		resp, err := http.ReadResponse(bufio.NewReaderSize(conn, 1), req)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("read CONNECT response: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("HTTP CONNECT to %s via %s: %s", addr, proxyAddr, resp.Status)
		}

		return conn, nil
	}
}
