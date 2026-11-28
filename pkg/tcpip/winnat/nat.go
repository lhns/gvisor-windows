// Package winnat is a user-space NAT for sandboxes on Windows hosts.
//
// The guest's netstack (the Sentry's) gets an eth0 whose other end is a
// second, small netstack (the gateway). The gateway terminates every guest
// TCP and UDP flow with netstack's forwarders and re-originates it as an
// ordinary host socket, so no driver, no admin rights and no host firewall
// rule is needed. This is the model of gvisor-tap-vsock (Podman's gvproxy).
// Inbound, Publish listens on 127.0.0.1 and opens a connection from the
// gateway into the guest.
//
// The package is plain Go (net and netstack only) and builds on any OS;
// only the resolver differs at run time.
//
// gvisor-windows: new package.
package winnat

import (
	"net/http"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/link/pipe"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// The virtual network, the same as QEMU's user networking.
var (
	// GuestIP is eth0's address in the guest.
	GuestIP = netip.MustParseAddr("10.0.2.15")
	// GatewayIP is the NAT's own address, the guest's default route.
	GatewayIP = netip.MustParseAddr("10.0.2.2")
	// DNSIP is the virtual DNS server: put it in the guest's resolv.conf.
	DNSIP = netip.MustParseAddr("10.0.2.3")
)

const (
	// PrefixLen of the virtual subnet 10.0.2.0/24.
	PrefixLen = 24
	// MTU of the guest's eth0.
	MTU = 1500

	guestMAC   = tcpip.LinkAddress("\x52\x54\x00\x12\x34\x56")
	gatewayMAC = tcpip.LinkAddress("\x52\x55\x0a\x00\x02\x02")
	nicID      = 1

	dialTimeout = 10 * time.Second
	udpIdle     = 60 * time.Second
	dnsIdle     = 10 * time.Second
)

// Options configure a NAT. The zero value is usable.
type Options struct {
	// Dialer makes the outbound host connections.
	Dialer *net.Dialer
	// Resolver answers the guest's DNS queries; net.DefaultResolver (the
	// Windows resolver) when nil.
	Resolver *net.Resolver
	// UpstreamDNS ("ip:port"), if set, makes the NAT forward the guest's DNS
	// queries unparsed to that server instead of using Resolver. For hosts where
	// the process must not open files (the Go resolver reads /etc/resolv.conf,
	// which a seccomp-confined Sentry cannot).
	UpstreamDNS string
	// HostLoopback maps connections to GatewayIP to 127.0.0.1 of the host,
	// so the guest can reach host services. Off by default: it exposes
	// everything listening on the host's loopback to the guest.
	HostLoopback bool
	// GatewayHTTP, if set, serves HTTP at GatewayIP:80 instead of dialing the
	// host: the guest's services API of the gateway (Podman's port
	// forwarding request, see runsc/boot/network_winnat.go).
	GatewayHTTP http.Handler
}

// NAT is the gateway side of the virtual network.
type NAT struct {
	opts    Options
	gw      *stack.Stack
	guestEP stack.LinkEndpoint

	mu        sync.Mutex
	listeners []net.Listener
	published map[uint16]net.Listener // by host port, for Unpublish
	gwConns   chan net.Conn

	closed    bool
}

func addr(a netip.Addr) tcpip.Address { return tcpip.AddrFromSlice(a.AsSlice()) }

func subnet(cidr string) tcpip.Subnet {
	p := netip.MustParsePrefix(cidr)
	bits := p.Addr().BitLen()
	mask := make([]byte, bits/8)
	for i := 0; i < p.Bits(); i++ {
		mask[i/8] |= 0x80 >> (i % 8)
	}
	s, err := tcpip.NewSubnet(addr(p.Masked().Addr()), tcpip.MaskFromBytes(mask))
	if err != nil {
		panic(err)
	}
	return s
}

// NewStack returns a netstack with the protocols a guest needs. Tests use it
// as the guest; the Sentry builds its own.
func NewStack(clock tcpip.Clock) *stack.Stack {
	return stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
		Clock:              clock,
		HandleLocal:        true,
	})
}

// New creates the gateway. Attach the guest with AttachGuest.
func New(opts Options) (*NAT, error) {
	if opts.Dialer == nil {
		opts.Dialer = &net.Dialer{Timeout: dialTimeout}
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	// A pipe's MTU includes the Ethernet header.
	guestEnd, gwEnd := pipe.New(guestMAC, gatewayMAC, MTU+header.EthernetMinimumSize)
	n := &NAT{opts: opts, guestEP: ethernet.New(guestEnd)}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	n.gw = s
	if err := s.CreateNIC(nicID, ethernet.New(gwEnd)); err != nil {
		return nil, fmt.Errorf("gateway NIC: %v", err)
	}
	// Promiscuous plus spoofing: accept packets for any destination and
	// answer from that destination, which is what makes the forwarders
	// look like the remote host to the guest.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return nil, fmt.Errorf("promiscuous: %v", err)
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return nil, fmt.Errorf("spoofing: %v", err)
	}
	for _, a := range []netip.Addr{GatewayIP, DNSIP} {
		pa := tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr(a), PrefixLen: PrefixLen},
		}
		if err := s.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
			return nil, fmt.Errorf("gateway address %v: %v", a, err)
		}
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	sack := tcpip.TCPSACKEnabled(true)
	s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)

	tcpFwd := tcp.NewForwarder(s, 0, 1024, n.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(s, n.handleUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)
	return n, nil
}

// AttachGuest gives the guest stack its lo and eth0 (10.0.2.15/24, default
// route via GatewayIP) and replaces its route table.
func (n *NAT) AttachGuest(s *stack.Stack) error {
	loID := s.NextNICID()
	if err := s.CreateNICWithOptions(loID, ethernet.NewWithNetworkMTU(loopback.New()), stack.NICOptions{Name: "lo", DeliverLinkPackets: true}); err != nil {
		return fmt.Errorf("lo: %v", err)
	}
	for _, pa := range []tcpip.ProtocolAddress{
		{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr(netip.MustParseAddr("127.0.0.1")), PrefixLen: 8}},
		{Protocol: ipv6.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr(netip.IPv6Loopback()), PrefixLen: 128}},
	} {
		if err := s.AddProtocolAddress(loID, pa, stack.AddressProperties{}); err != nil {
			return fmt.Errorf("lo address: %v", err)
		}
	}
	ethID := s.NextNICID()
	if err := s.CreateNICWithOptions(ethID, n.guestEP, stack.NICOptions{Name: "eth0", DeliverLinkPackets: true}); err != nil {
		return fmt.Errorf("eth0: %v", err)
	}
	pa := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr(GuestIP), PrefixLen: PrefixLen},
	}
	if err := s.AddProtocolAddress(ethID, pa, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("eth0 address: %v", err)
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: subnet("127.0.0.0/8"), NIC: loID},
		{Destination: subnet("::1/128"), NIC: loID},
		{Destination: subnet("10.0.2.0/24"), NIC: ethID},
		{Destination: header.IPv4EmptySubnet, Gateway: addr(GatewayIP), NIC: ethID},
	})
	return nil
}

// Close stops the published listeners and the gateway.
func (n *NAT) Close() {
	n.mu.Lock()
	n.closed = true
	ls := n.listeners
	n.listeners = nil
	n.mu.Unlock()
	for _, l := range ls {
		l.Close()
	}
	n.gw.Close()
}
