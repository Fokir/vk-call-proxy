package dtls

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

// Listener wraps a pion/dtls listener for the VPN server.
type Listener struct {
	ln       net.Listener
	udpConn  *net.UDPConn
	certPool tls.Certificate
}

// Listen creates a DTLS listener on the given UDP address.
// Uses a self-signed certificate with ECDSA-AES128-GCM-SHA256.
func Listen(addr string) (*Listener, error) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, fmt.Errorf("generate self-signed cert: %w", err)
	}

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve addr: %w", err)
	}

	config := &dtls.Config{
		Certificates:          []tls.Certificate{certificate},
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.RandomCIDGenerator(8),
	}

	ln, err := dtls.Listen("udp", udpAddr, config)
	if err != nil {
		return nil, fmt.Errorf("dtls listen: %w", err)
	}

	return &Listener{
		ln:       ln,
		certPool: certificate,
	}, nil
}

// Accept waits for and returns the next DTLS connection.
// Performs the DTLS handshake with a 30-second timeout.
func (l *Listener) Accept(ctx context.Context) (net.Conn, error) {
	conn, err := l.ln.Accept()
	if err != nil {
		return nil, err
	}

	dtlsConn, ok := conn.(*dtls.Conn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("unexpected connection type")
	}

	hsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := dtlsConn.HandshakeContext(hsCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("dtls handshake: %w", err)
	}

	return dtlsConn, nil
}

// Close shuts down the DTLS listener.
func (l *Listener) Close() error {
	return l.ln.Close()
}

// CertFingerprint returns the SHA-256 fingerprint of the server's DTLS certificate.
func (l *Listener) CertFingerprint() [32]byte {
	return sha256.Sum256(l.certPool.Certificate[0])
}
