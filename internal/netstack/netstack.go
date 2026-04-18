package netstack

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/call-vpn/call-vpn/internal/mux"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID   = 1
	mtu     = 1280 // match mobile TUN MTU; keeps DTLS records within TURN relay limits
	chanSz  = 512  // channel endpoint queue size
	tcpBuf  = 0    // use default receive window
	maxConn = 65535
)

// Stack wraps gVisor's userspace TCP/IP stack to process raw IP packets
// from mobile clients and proxy them to the real network.
type Stack struct {
	s           *stack.Stack
	ep          *channel.Endpoint
	m           *mux.Mux
	logger      *slog.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New creates a gVisor netstack that handles raw IP packets from the mux.
// TCP and UDP connections are forwarded to real destinations via net.Dial.
// If dialContext is non-nil, it is used to dial TCP connections (e.g. through an upstream proxy).
func New(logger *slog.Logger, m *mux.Mux, dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) *Stack {
	ep := channel.New(chanSz, mtu, "")

	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
			icmp.NewProtocol6,
		},
	})

	if err := s.CreateNIC(nicID, ep); err != nil {
		logger.Error("create NIC failed", "err", err)
		return nil
	}

	// Enable promiscuous mode so the stack accepts packets for any destination.
	s.SetPromiscuousMode(nicID, true)
	// Enable spoofing so the stack can send from any source address.
	s.SetSpoofing(nicID, true)

	// Add default routes for IPv4 and IPv6.
	s.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	ns := &Stack{
		s:           s,
		ep:          ep,
		m:           m,
		logger:      logger.With("component", "netstack"),
		dialContext: dialContext,
	}

	// Set up TCP forwarder.
	tcpFwd := tcp.NewForwarder(s, tcpBuf, maxConn, ns.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	// Set up UDP forwarder.
	udpFwd := udp.NewForwarder(s, ns.handleUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	return ns
}

// Start begins processing raw IP packets in both directions.
// Inbound: mux RawPackets → inject into gVisor stack.
// Outbound: gVisor stack → send as FrameData{StreamID:0} via mux.
func (ns *Stack) Start(ctx context.Context) {
	ns.ctx, ns.cancel = context.WithCancel(ctx)

	ns.wg.Add(2)
	go ns.inboundLoop()
	go ns.outboundLoop()

	ns.logger.Info("netstack started")
}

// Close shuts down the netstack.
func (ns *Stack) Close() {
	if ns.cancel != nil {
		ns.cancel()
	}
	ns.wg.Wait()
	ns.ep.Close()
	ns.s.Close()
	ns.logger.Info("netstack closed")
}

// inboundLoop reads raw IP packets from the mux and injects them into gVisor.
func (ns *Stack) inboundLoop() {
	defer ns.wg.Done()
	rb := ns.m.RawPackets()
	if rb == nil {
		return
	}
	for {
		// Drain all available frames before waiting.
		for {
			f, ok := rb.Pop()
			if !ok {
				break
			}
			ns.injectPacket(f.Payload)
		}

		select {
		case _, ok := <-rb.Ready():
			if !ok {
				return
			}
		case <-ns.ctx.Done():
			return
		}
	}
}

// injectPacket parses the IP version and injects the packet into the stack.
// ICMP echo requests are handled directly without entering gVisor.
func (ns *Stack) injectPacket(data []byte) {
	if len(data) == 0 {
		return
	}

	switch header.IPVersion(data) {
	case 4:
		if ns.handleICMPv4Echo(data) {
			return
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(data),
		})
		ns.ep.InjectInbound(header.IPv4ProtocolNumber, pkt)
		pkt.DecRef()
	case 6:
		if ns.handleICMPv6Echo(data) {
			return
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(data),
		})
		ns.ep.InjectInbound(header.IPv6ProtocolNumber, pkt)
		pkt.DecRef()
	}
}

// handleICMPv4Echo intercepts ICMPv4 echo requests and generates replies
// directly, without forwarding to a real destination. This verifies VPN
// tunnel connectivity and works without CAP_NET_RAW.
func (ns *Stack) handleICMPv4Echo(data []byte) bool {
	if len(data) < header.IPv4MinimumSize {
		return false
	}
	ipHdr := header.IPv4(data)
	if ipHdr.TransportProtocol() != header.ICMPv4ProtocolNumber {
		return false
	}
	hdrLen := int(ipHdr.HeaderLength())
	if len(data) < hdrLen+header.ICMPv4MinimumSize {
		return false
	}
	icmpHdr := header.ICMPv4(data[hdrLen:])
	if icmpHdr.Type() != header.ICMPv4Echo {
		return false
	}

	reply := make([]byte, len(data))
	copy(reply, data)

	replyIP := header.IPv4(reply)
	replyICMP := header.ICMPv4(reply[hdrLen:])

	// Swap src ↔ dst.
	src := ipHdr.SourceAddress()
	dst := ipHdr.DestinationAddress()
	replyIP.SetSourceAddress(dst)
	replyIP.SetDestinationAddress(src)
	replyIP.SetTTL(64)

	// Echo → EchoReply.
	replyICMP.SetType(header.ICMPv4EchoReply)
	replyICMP.SetChecksum(0)
	replyICMP.SetChecksum(header.ICMPv4Checksum(replyICMP, 0))

	// Recalculate IP checksum.
	replyIP.SetChecksum(0)
	replyIP.SetChecksum(^replyIP.CalculateChecksum())

	_ = ns.m.SendRawPacket(&mux.Frame{
		StreamID: 0,
		Type:     mux.FrameData,
		Sequence: ns.m.NextSeq(),
		Length:   uint32(len(reply)),
		Payload:  reply,
	})
	return true
}

// handleICMPv6Echo intercepts ICMPv6 echo requests and generates replies.
func (ns *Stack) handleICMPv6Echo(data []byte) bool {
	if len(data) < header.IPv6MinimumSize {
		return false
	}
	ipHdr := header.IPv6(data)
	if ipHdr.TransportProtocol() != header.ICMPv6ProtocolNumber {
		return false
	}
	hdrLen := header.IPv6MinimumSize
	if len(data) < hdrLen+header.ICMPv6MinimumSize {
		return false
	}
	icmpHdr := header.ICMPv6(data[hdrLen:])
	if icmpHdr.Type() != header.ICMPv6EchoRequest {
		return false
	}

	reply := make([]byte, len(data))
	copy(reply, data)

	replyIP := header.IPv6(reply)
	replyICMP := header.ICMPv6(reply[hdrLen:])

	// Swap src ↔ dst.
	src := ipHdr.SourceAddress()
	dst := ipHdr.DestinationAddress()
	replyIP.SetSourceAddress(dst)
	replyIP.SetDestinationAddress(src)
	replyIP.SetHopLimit(64)

	// EchoRequest → EchoReply.
	replyICMP.SetType(header.ICMPv6EchoReply)

	// ICMPv6 checksum includes pseudo-header.
	replyICMP.SetChecksum(0)
	replyICMP.SetChecksum(header.ICMPv6Checksum(header.ICMPv6ChecksumParams{
		Header:      replyICMP,
		Src:         dst,
		Dst:         src,
		PayloadCsum: 0,
		PayloadLen:  len(replyICMP) - header.ICMPv6MinimumSize,
	}))

	_ = ns.m.SendRawPacket(&mux.Frame{
		StreamID: 0,
		Type:     mux.FrameData,
		Sequence: ns.m.NextSeq(),
		Length:   uint32(len(reply)),
		Payload:  reply,
	})
	return true
}

// outboundLoop reads packets from the gVisor stack and sends them via mux.
func (ns *Stack) outboundLoop() {
	defer ns.wg.Done()
	for {
		pkt := ns.ep.ReadContext(ns.ctx)
		if pkt == nil {
			return
		}

		view := pkt.ToView()
		data := view.AsSlice()
		// Make a copy since view will be invalidated.
		buf := make([]byte, len(data))
		copy(buf, data)
		view.Release()
		pkt.DecRef()

		_ = ns.m.SendRawPacket(&mux.Frame{
			StreamID: 0,
			Type:     mux.FrameData,
			Sequence: ns.m.NextSeq(),
			Length:   uint32(len(buf)),
			Payload:  buf,
		})
	}
}

// handleTCP is called by the TCP forwarder for each new TCP connection.
func (ns *Stack) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))

	// Dial real destination.
	var outConn net.Conn
	var err error
	if ns.dialContext != nil {
		dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dialCancel()
		outConn, err = ns.dialContext(dialCtx, "tcp", dst)
	} else {
		outConn, err = net.DialTimeout("tcp", dst, 10*time.Second)
	}
	if err != nil {
		ns.logger.Debug("tcp dial failed", "dst", dst, "err", err)
		r.Complete(true) // send RST
		return
	}

	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		ns.logger.Debug("tcp create endpoint failed", "dst", dst, "err", tcpErr)
		r.Complete(true)
		outConn.Close()
		return
	}
	r.Complete(false)
	conn := gonet.NewTCPConn(&wq, ep)

	ns.logger.Debug("tcp relay", "dst", dst)
	go relay(conn, outConn)
}

// handleUDP is called by the UDP forwarder for each new UDP flow.
func (ns *Stack) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort)))

	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		ns.logger.Debug("udp create endpoint failed", "dst", dst, "err", err)
		return true
	}
	conn := gonet.NewUDPConn(&wq, ep)

	outConn, dialErr := net.Dial("udp", dst)
	if dialErr != nil {
		ns.logger.Debug("udp dial failed", "dst", dst, "err", dialErr)
		conn.Close()
		return true
	}

	ns.logger.Debug("udp relay", "dst", dst)
	go relayUDP(conn, outConn)
	return true
}

// relay copies data bidirectionally between two TCP connections.
func relay(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(b, a)
	}()
	go func() {
		defer wg.Done()
		io.Copy(a, b)
	}()
	wg.Wait()
}

// relayUDP copies data bidirectionally between a gVisor UDP conn and a real UDP conn.
func relayUDP(gvConn net.Conn, outConn net.Conn) {
	defer gvConn.Close()
	defer outConn.Close()

	const udpTimeout = 2 * time.Minute

	var wg sync.WaitGroup
	wg.Add(2)

	// gVisor → real network.
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			gvConn.SetReadDeadline(time.Now().Add(udpTimeout))
			n, err := gvConn.Read(buf)
			if err != nil {
				return
			}
			outConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			outConn.Write(buf[:n])
		}
	}()

	// Real network → gVisor.
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			outConn.SetReadDeadline(time.Now().Add(udpTimeout))
			n, err := outConn.Read(buf)
			if err != nil {
				return
			}
			gvConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			gvConn.Write(buf[:n])
		}
	}()

	wg.Wait()
}
