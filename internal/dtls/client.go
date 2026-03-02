package dtls

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

// DialOverTURN establishes a DTLS connection to serverAddr through
// a TURN relay PacketConn. It uses an AsyncPacketPipe to bridge the
// datagram-based TURN relay with the DTLS handshake layer.
//
// Returns a net.Conn that implements io.ReadWriteCloser, suitable
// for plugging directly into the MUX as a stream-oriented connection.
// The returned CancelFunc must be called to stop the bridge goroutines.
func DialOverTURN(ctx context.Context, relayConn net.PacketConn, serverAddr *net.UDPAddr) (net.Conn, context.CancelFunc, error) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, nil, fmt.Errorf("generate self-signed cert: %w", err)
	}

	// AsyncPacketPipe creates a pair of connected PacketConns.
	// Writes to conn1 are reads on conn2 and vice versa.
	conn1, conn2 := connutil.AsyncPacketPipe()

	bridgeCtx, bridgeCancel := context.WithCancel(ctx)

	// Bridge conn2 <-> relayConn (two goroutines).
	var wg sync.WaitGroup
	wg.Add(2)

	// relay -> pipe: read from TURN relay, write to pipe
	go func() {
		defer wg.Done()
		defer bridgeCancel()
		buf := make([]byte, 65535)
		for {
			select {
			case <-bridgeCtx.Done():
				return
			default:
			}
			n, _, err := relayConn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, err = conn2.WriteTo(buf[:n], serverAddr)
			if err != nil {
				return
			}
		}
	}()

	// pipe -> relay: read from pipe, write to TURN relay targeting serverAddr
	go func() {
		defer wg.Done()
		defer bridgeCancel()
		buf := make([]byte, 65535)
		for {
			select {
			case <-bridgeCtx.Done():
				return
			default:
			}
			n, _, err := conn2.ReadFrom(buf)
			if err != nil {
				return
			}
			_, err = relayConn.WriteTo(buf[:n], serverAddr)
			if err != nil {
				return
			}
		}
	}()

	// Set deadlines on cancel to unblock reads.
	context.AfterFunc(bridgeCtx, func() {
		relayConn.SetDeadline(time.Now())
		conn2.SetDeadline(time.Now())
	})

	// DTLS client handshake over conn1 (the other end of the pipe).
	config := &dtls.Config{
		Certificates:          []tls.Certificate{certificate},
		InsecureSkipVerify:    true,
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.OnlySendCIDGenerator(),
	}

	hsCtx, hsCancel := context.WithTimeout(ctx, 30*time.Second)
	defer hsCancel()

	dtlsConn, err := dtls.Client(conn1, serverAddr, config)
	if err != nil {
		bridgeCancel()
		wg.Wait()
		conn1.Close()
		conn2.Close()
		return nil, nil, fmt.Errorf("dtls client create: %w", err)
	}

	if err := dtlsConn.HandshakeContext(hsCtx); err != nil {
		dtlsConn.Close()
		bridgeCancel()
		wg.Wait()
		conn1.Close()
		conn2.Close()
		return nil, nil, fmt.Errorf("dtls handshake: %w", err)
	}

	// Wrap cleanup: cancel bridge, wait for goroutines, close pipes.
	cleanup := func() {
		dtlsConn.Close()
		bridgeCancel()
		wg.Wait()
		conn1.Close()
		conn2.Close()
	}

	return dtlsConn, cleanup, nil
}
