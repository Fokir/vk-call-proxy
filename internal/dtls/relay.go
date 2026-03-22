package dtls

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/elliptic"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

// punchByte is the legacy single-byte probe value, kept for backward-compat detection.
const punchByte = 0x00

// buildPunchProbe creates a 20-byte STUN-like packet used as a punch probe.
// The format mimics a STUN Shared Secret request (method 0x0002, deprecated/unhandled by pion)
// so it blends in with STUN traffic and is unlikely to be processed by TURN servers.
func buildPunchProbe() []byte {
	buf := make([]byte, 20)
	buf[0], buf[1] = 0x00, 0x02                      // STUN Shared Secret method
	// bytes 2-3: message length = 0
	buf[4], buf[5], buf[6], buf[7] = 0x21, 0x12, 0xA4, 0x42 // STUN Magic Cookie
	rand.Read(buf[8:20])                              // random Transaction ID
	return buf
}

// isPunch returns true if the packet is a PunchRelay probe.
// Accepts both the legacy single-byte format and the new STUN-like 20-byte format.
func isPunch(buf []byte, n int) bool {
	if n == 1 && buf[0] == 0x00 {
		return true // legacy single-byte probe
	}
	return n == 20 && buf[0] == 0x00 && buf[1] == 0x02 &&
		buf[4] == 0x21 && buf[5] == 0x12 && buf[6] == 0xA4 && buf[7] == 0x42
}

// bridgePipeBufferSize limits the AsyncPacketPipe buffer between DTLS and
// the TURN relay bridge. Without a limit, burst writes fill the pipe faster
// than the bridge can drain to the TURN relay, causing the inter-server
// UDP relay to overflow and drop packets.
const bridgePipeBufferSize = 512 * 1024

// Adaptive pacing constants for the pipe→relay bridge.
// When WriteTo completes quickly (buffer has space), we add a small minimum
// sleep to avoid overwhelming the TURN relay's internal forwarding.
// When WriteTo blocks (TCP backpressure from filled 16KB write buffer),
// no extra sleep is needed — the network is already rate-limiting us.
const (
	bridgeMinPace      = 1 * time.Millisecond          // minimum pace when writes are instant
	bridgeSlowWrite    = 500 * time.Microsecond         // if write took this long, skip extra sleep
)

// AcceptOverTURN establishes a server-side DTLS connection through a TURN
// relay PacketConn. Mirrors DialOverTURN but uses dtls.Server() instead
// of dtls.Client(). The clientRelayAddr is the remote peer's relay address.
func AcceptOverTURN(ctx context.Context, relayConn net.PacketConn, clientRelayAddr *net.UDPAddr) (net.Conn, context.CancelFunc, error) {
	// Server side uses ECDSA cert — compatible with both ECDSA and RSA cipher suites
	// from old and new clients. ClientHello fingerprint only matters for DialOverTURN.
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		return nil, nil, fmt.Errorf("generate self-signed cert: %w", err)
	}

	conn1, conn2 := connutil.LimitedAsyncPacketPipe(bridgePipeBufferSize)

	// Use Background so the bridge outlives the caller's ctx (which may be
	// a short-lived timeout used only for the handshake). The cleanup func
	// returned to the caller is the sole way to tear down the bridge.
	bridgeCtx, bridgeCancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(2)

	// relay -> pipe (filter out punch probes)
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
			if isPunch(buf, n) {
				continue
			}
			_, err = conn2.WriteTo(buf[:n], clientRelayAddr)
			if err != nil {
				return
			}
		}
	}()

	// pipe -> relay (adaptive pacing)
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
			start := time.Now()
			_, err = relayConn.WriteTo(buf[:n], clientRelayAddr)
			if err != nil {
				return
			}
			// Adaptive: if write was fast (buffer not full), pace to avoid
			// overwhelming the relay. If write blocked (TCP backpressure),
			// no extra sleep needed.
			if elapsed := time.Since(start); elapsed < bridgeSlowWrite {
				time.Sleep(bridgeMinPace)
			}
		}
	}()

	context.AfterFunc(bridgeCtx, func() {
		relayConn.SetDeadline(time.Now())
		conn2.SetDeadline(time.Now())
	})

	// DTLS server handshake over conn1.
	config := &dtls.Config{
		Certificates:         []tls.Certificate{certificate},
		ExtendedMasterSecret: dtls.RequireExtendedMasterSecret,
		CipherSuites: []dtls.CipherSuiteID{
			dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			dtls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		},
		EllipticCurves: []elliptic.Curve{
			elliptic.X25519,
			elliptic.P256,
			elliptic.P384,
		},
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{
			dtls.SRTP_AES128_CM_HMAC_SHA1_80,
		},
		ConnectionIDGenerator: dtls.RandomCIDGenerator(8),
	}

	// Use 20s default handshake timeout; if ctx has an earlier deadline, honour it.
	// Close the pipe end when the timeout fires to force dtls.Server to abort.
	hsCtx, hsCancel := context.WithTimeout(ctx, 20*time.Second)
	defer hsCancel()
	stopHS := context.AfterFunc(hsCtx, func() {
		conn1.Close()
	})

	dtlsConn, err := dtls.Server(conn1, clientRelayAddr, config)
	stopHS() // cancel the AfterFunc if handshake succeeded in time
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
// a TURN permission. Retries up to 3 times with 100ms gaps on failure.
// Both sides must call this before DTLS handshake.
func PunchRelay(relayConn net.PacketConn, remoteAddr *net.UDPAddr) error {
	var lastErr error
	for i := 0; i < 3; i++ {
		if i > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		_, lastErr = relayConn.WriteTo(buildPunchProbe(), remoteAddr)
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// StartPunchLoop sends periodic probe packets to keep TURN permissions
// alive during the DTLS handshake. Stops when ctx is cancelled.
// Phase 1 (first 2s): 50ms interval for fast hole-punching.
// Phase 2 (after 2s): 200ms interval for steady-state keepalive.
func StartPunchLoop(ctx context.Context, relayConn net.PacketConn, remoteAddr *net.UDPAddr) {
	fast := time.NewTicker(50 * time.Millisecond)
	timer := time.NewTimer(2 * time.Second)
	defer fast.Stop()
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			goto steady
		case <-fast.C:
			relayConn.WriteTo(buildPunchProbe(), remoteAddr)
		}
	}
steady:
	slow := time.NewTicker(200 * time.Millisecond)
	defer slow.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-slow.C:
			relayConn.WriteTo(buildPunchProbe(), remoteAddr)
		}
	}
}
