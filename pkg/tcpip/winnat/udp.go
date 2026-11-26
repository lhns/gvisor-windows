package winnat

import (
	"net"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// handleUDP creates one connected endpoint per guest flow (source and
// destination address and port). Flows to the virtual DNS server are
// answered here; the others get a host UDP socket and live until idle.
func (n *NAT) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	wq := new(waiter.Queue)
	ep, err := r.CreateEndpoint(wq)
	if err != nil {
		return false
	}
	guest := gonet.NewUDPConn(wq, ep)
	if id.LocalPort == 53 && (id.LocalAddress == addr(DNSIP) || id.LocalAddress == addr(GatewayIP)) {
		go n.serveDNS(guest)
		return true
	}
	dst := n.hostAddr(id.LocalAddress, id.LocalPort)
	go func() {
		host, err := n.opts.Dialer.Dial("udp", dst)
		if err != nil {
			guest.Close()
			return
		}
		relayUDP(guest, host)
	}()
	return true
}

// relayUDP copies datagrams both ways and closes both after udpIdle without
// traffic in either direction.
func relayUDP(a, b net.Conn) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 65536)
		for {
			src.SetReadDeadline(time.Now().Add(time.Second))
			m, err := src.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() &&
					time.Since(time.Unix(0, last.Load())) < udpIdle {
					continue
				}
				return
			}
			last.Store(time.Now().UnixNano())
			if _, err := dst.Write(buf[:m]); err != nil {
				return
			}
		}
	}
	go cp(b, a)
	go cp(a, b)
	<-done
	a.Close()
	b.Close()
	<-done
}
