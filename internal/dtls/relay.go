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

// AcceptOverTURN establishes a server-side DTLS connection through a TURN
// relay PacketConn. Mirrors DialOverTURN but uses dtls.Server() instead
// of dtls.Client(). The clientRelayAddr is the remote peer's relay address.
func AcceptOverTURN(ctx context.Context, relayConn net.PacketConn, clientRelayAddr *net.UDPAddr) (net.Conn, context.CancelFunc, error) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, nil, fmt.Errorf("generate self-signed cert: %w", err)
	}

	conn1, conn2 := connutil.AsyncPacketPipe()

	bridgeCtx, bridgeCancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	wg.Add(2)

	// relay -> pipe
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
			_, err = conn2.WriteTo(buf[:n], clientRelayAddr)
			if err != nil {
				return
			}
		}
	}()

	// pipe -> relay
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
			_, err = relayConn.WriteTo(buf[:n], clientRelayAddr)
			if err != nil {
				return
			}
		}
	}()

	context.AfterFunc(bridgeCtx, func() {
		relayConn.SetDeadline(time.Now())
		conn2.SetDeadline(time.Now())
	})

	// DTLS server handshake over conn1.
	config := &dtls.Config{
		Certificates:          []tls.Certificate{certificate},
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.RandomCIDGenerator(8),
	}

	hsCtx, hsCancel := context.WithTimeout(ctx, 30*time.Second)
	defer hsCancel()

	dtlsConn, err := dtls.Server(conn1, clientRelayAddr, config)
	if err != nil {
		bridgeCancel()
		wg.Wait()
		conn1.Close()
		conn2.Close()
		return nil, nil, fmt.Errorf("dtls server create: %w", err)
	}

	if err := dtlsConn.HandshakeContext(hsCtx); err != nil {
		dtlsConn.Close()
		bridgeCancel()
		wg.Wait()
		conn1.Close()
		conn2.Close()
		return nil, nil, fmt.Errorf("dtls handshake: %w", err)
	}

	cleanup := func() {
		dtlsConn.Close()
		bridgeCancel()
		wg.Wait()
		conn1.Close()
		conn2.Close()
	}

	return dtlsConn, cleanup, nil
}

// PunchRelay sends a probe packet to the remote relay address to create
// a TURN permission. Both sides must call this before DTLS handshake.
func PunchRelay(relayConn net.PacketConn, remoteAddr *net.UDPAddr) error {
	_, err := relayConn.WriteTo([]byte{0x00}, remoteAddr)
	return err
}
