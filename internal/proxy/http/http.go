package http

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// DialFunc establishes a connection to the target through the mux tunnel.
type DialFunc func(ctx context.Context, network, addr string) (io.ReadWriteCloser, error)

// Server implements an HTTP/HTTPS proxy with CONNECT support.
type Server struct {
	Addr   string
	Dial   DialFunc
	Logger *slog.Logger
	server *http.Server
}

// ListenAndServe starts the HTTP proxy server.
func (s *Server) ListenAndServe(ctx context.Context) error {
	s.server = &http.Server{
		Addr:              s.Addr,
		Handler:           s,
		ReadHeaderTimeout: 30 * time.Second,
	}

	s.Logger.Info("HTTP proxy listening", "addr", s.Addr)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(shutCtx)
	}()

	err := s.server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Close stops the server.
func (s *Server) Close() error {
	if s.server != nil {
		return s.server.Close()
	}
	return nil
}

// ServeHTTP handles both CONNECT (tunneling) and regular HTTP proxy requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
	} else {
		s.handleHTTP(w, r)
	}
}

// handleConnect implements the HTTP CONNECT method for HTTPS tunneling.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	remote, err := s.Dial(r.Context(), "tcp", r.Host)
	if err != nil {
		s.Logger.Warn("CONNECT dial failed", "host", r.Host, "err", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer remote.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	client, _, err := hj.Hijack()
	if err != nil {
		s.Logger.Warn("hijack failed", "err", err)
		return
	}
	defer client.Close()

	// Clear any deadlines inherited from the HTTP server so the
	// CONNECT tunnel can stay open indefinitely.
	if d, ok := client.(interface{ SetDeadline(time.Time) error }); ok {
		d.SetDeadline(time.Time{})
	}

	// Send 200 Connection Established
	fmt.Fprintf(client, "HTTP/1.1 200 Connection Established\r\n\r\n")

	// Bidirectional relay
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(remote, client)
	}()

	go func() {
		defer wg.Done()
		io.Copy(client, remote)
	}()

	wg.Wait()
}

// handleHTTP proxies regular HTTP requests through the tunnel.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Host == "" {
		http.Error(w, "Missing host", http.StatusBadRequest)
		return
	}

	host := r.URL.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "80")
	}

	remote, err := s.Dial(r.Context(), "tcp", host)
	if err != nil {
		s.Logger.Warn("HTTP dial failed", "host", host, "err", err)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer remote.Close()

	// Remove hop-by-hop headers
	r.Header.Del("Proxy-Connection")
	r.Header.Del("Proxy-Authenticate")
	r.Header.Del("Proxy-Authorization")

	// Write the request to the remote connection
	if err := r.Write(remote); err != nil {
		http.Error(w, "Error writing request", http.StatusBadGateway)
		return
	}

	// Read response and relay back
	resp, err := http.ReadResponse(bufio.NewReader(remote), r)
	if err != nil {
		http.Error(w, "Error reading response", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy response headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
