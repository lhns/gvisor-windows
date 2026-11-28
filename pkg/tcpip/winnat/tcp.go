package winnat

import (
	"fmt"
	"net/http"
	"context"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// hostAddr is the host address a guest flow to (a, port) is re-originated to.
func (n *NAT) hostAddr(a tcpip.Address, port uint16) string {
	ip, _ := netip.AddrFromSlice(a.AsSlice())
	if n.opts.HostLoopback && ip == GatewayIP {
		ip = netip.MustParseAddr("127.0.0.1")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
}

// handleTCP re-originates a guest connection as a host connection. The
// host connection is made first, so a refused or unreachable destination
// gives the guest an RST instead of a connection that then dies.
func (n *NAT) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	if n.opts.GatewayHTTP != nil && id.LocalPort == 80 && id.LocalAddress == addr(GatewayIP) {
		go n.handleGatewayHTTP(r)
		return
	}
	dst := n.hostAddr(id.LocalAddress, id.LocalPort)
	go func() {
		host, err := n.opts.Dialer.Dial("tcp", dst)
		if err != nil {
			r.Complete(true)
			return
		}
		wq := new(waiter.Queue)
		ep, terr := r.CreateEndpoint(wq)
		if terr != nil {
			r.Complete(true)
			host.Close()
			return
		}
		r.Complete(false)
		relay(gonet.NewTCPConn(wq, ep), host)
	}()
}

// Publish forwards 127.0.0.1:hostPort (0 picks a free port) to guestPort of
// the guest, and returns the host port. Only the loopback address is
// bound: no firewall prompt, no exposure to the network.
func (n *NAT) Publish(hostPort, guestPort uint16) (uint16, error) {
	// No MPTCP: Go tries it first for listeners, and a seccomp-confined Sentry
	// does not allow socket(..., IPPROTO_MPTCP).
	var lc net.ListenConfig
	lc.SetMultipathTCP(false)
	l, err := lc.Listen(context.Background(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(hostPort))))
	if err != nil {
		return 0, err
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		l.Close()
		return 0, net.ErrClosed
	}
	n.listeners = append(n.listeners, l)
	if n.published == nil {
		n.published = map[uint16]net.Listener{}
	}
	n.published[uint16(l.Addr().(*net.TCPAddr).Port)] = l
	n.mu.Unlock()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
				defer cancel()
				g, err := gonet.DialContextTCP(ctx, n.gw, tcpip.FullAddress{Addr: addr(GuestIP), Port: guestPort}, ipv4.ProtocolNumber)
				if err != nil {
					c.Close()
					return
				}
				relay(c, g)
			}()
		}
	}()
	return uint16(l.Addr().(*net.TCPAddr).Port), nil
}

// relay copies both ways until both directions end, passing half-closes on;
// an error in either direction tears both down.
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		if _, err := io.Copy(dst, src); err != nil {
			a.Close()
			b.Close()
			return
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// DialGuest opens a connection from the gateway to a TCP port of the guest,
// for host-side services that are not a published loopback port (the Windows
// named-pipe forwarder of runsc), so no other local user can reach them.
func (n *NAT) DialGuest(guestPort uint16) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	return gonet.DialContextTCP(ctx, n.gw, tcpip.FullAddress{Addr: addr(GuestIP), Port: guestPort}, ipv4.ProtocolNumber)
}

// Relay copies between a and b until both directions end, then closes both.
func Relay(a, b net.Conn) { relay(a, b) }

// Unpublish stops the listener Publish made for hostPort.
func (n *NAT) Unpublish(hostPort uint16) error {
	n.mu.Lock()
	l := n.published[hostPort]
	delete(n.published, hostPort)
	n.mu.Unlock()
	if l == nil {
		return fmt.Errorf("host port %d is not published", hostPort)
	}
	return l.Close()
}

// handleGatewayHTTP serves a guest connection to GatewayIP:80 with
// Options.GatewayHTTP, over an in-memory connection.
func (n *NAT) handleGatewayHTTP(r *tcp.ForwarderRequest) {
	wq := new(waiter.Queue)
	ep, terr := r.CreateEndpoint(wq)
	if terr != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)
	a, b := net.Pipe()
	n.mu.Lock()
	if n.gwConns == nil {
		n.gwConns = make(chan net.Conn)
		go (&http.Server{Handler: n.opts.GatewayHTTP}).Serve(chanListener(n.gwConns))
	}
	ch := n.gwConns
	n.mu.Unlock()
	ch <- b
	relay(gonet.NewTCPConn(wq, ep), a)
}

type chanListener chan net.Conn

func (l chanListener) Accept() (net.Conn, error) {
	c, ok := <-l
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (chanListener) Close() error   { return nil }
func (chanListener) Addr() net.Addr { return &net.TCPAddr{} }
