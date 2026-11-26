package winnat_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/winnat"
)

// The tests play the guest themselves: a netstack with the NAT's eth0, driven
// through gonet. They need internet access (github.com) and nothing else.

func newGuest(t *testing.T, opts winnat.Options) (*winnat.NAT, *stack.Stack) {
	t.Helper()
	nat, err := winnat.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nat.Close)
	s := winnat.NewStack(nil)
	if err := nat.AttachGuest(s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return nat, s
}

func fullAddr(ip string, port uint16) tcpip.FullAddress {
	return tcpip.FullAddress{Addr: tcpip.AddrFromSlice(net.ParseIP(ip).To4()), Port: port}
}

// guestResolver is a resolver inside the guest that asks the virtual DNS
// server, as the guest's libc would with resolv.conf pointing at it.
func guestResolver(s *stack.Stack) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		if !strings.HasPrefix(network, "udp") {
			return nil, fmt.Errorf("only UDP DNS, got %s", network)
		}
		a := fullAddr(winnat.DNSIP.String(), 53)
		return gonet.DialUDP(s, nil, &a, ipv4.ProtocolNumber)
	}}
}

func query(t *testing.T, s *stack.Stack, name string, typ dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	n, err := dnsmessage.NewName(name)
	if err != nil {
		t.Fatal(err)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: n, Type: typ, Class: dnsmessage.ClassINET})
	raw, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	a := fullAddr(winnat.DNSIP.String(), 53)
	c, err := gonet.DialUDP(s, nil, &a, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write(raw); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	m, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no DNS answer for %s: %v", name, err)
	}
	var msg dnsmessage.Message
	if err := msg.Unpack(buf[:m]); err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestDNS(t *testing.T) {
	_, s := newGuest(t, winnat.Options{})
	msg := query(t, s, "github.com.", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeSuccess || len(msg.Answers) == 0 {
		t.Fatalf("github.com A: rcode %v, %d answers", msg.RCode, len(msg.Answers))
	}
	t.Logf("github.com A: %v", msg.Answers[0].Body)

	msg = query(t, s, "does-not-exist.invalid.", dnsmessage.TypeA)
	if msg.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("does-not-exist.invalid: rcode %v, want NXDOMAIN", msg.RCode)
	}

	// The same through a resolver implementation that is not ours.
	ips, err := guestResolver(s).LookupNetIP(context.Background(), "ip4", "github.com")
	if err != nil || len(ips) == 0 {
		t.Fatalf("guest resolver: %v %v", ips, err)
	}
}

func TestHTTPS(t *testing.T) {
	_, s := newGuest(t, winnat.Options{})
	res := guestResolver(s)
	client := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := res.LookupNetIP(ctx, "ip4", host)
			if err != nil {
				return nil, err
			}
			var p uint16
			fmt.Sscan(port, &p)
			return gonet.DialContextTCP(ctx, s, fullAddr(ips[0].String(), p), ipv4.ProtocolNumber)
		}},
	}
	resp, err := client.Get("https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body)
	t.Logf("GET https://github.com: %s, %d bytes", resp.Status, n)
	if resp.StatusCode != http.StatusOK || n == 0 {
		t.Fatalf("status %s, %d bytes", resp.Status, n)
	}
}

func TestUDPRoundTrip(t *testing.T) {
	_, s := newGuest(t, winnat.Options{HostLoopback: true})
	echo, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFrom(buf)
			if err != nil {
				return
			}
			echo.WriteTo([]byte(strings.ToUpper(string(buf[:n]))), from)
		}
	}()

	a := fullAddr(winnat.GatewayIP.String(), uint16(echo.LocalAddr().(*net.UDPAddr).Port))
	c, err := gonet.DialUDP(s, nil, &a, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 3; i++ {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		msg := fmt.Sprintf("ping %d", i)
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(buf[:n]), strings.ToUpper(msg); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestTCPRefusedAndAccepted(t *testing.T) {
	_, s := newGuest(t, winnat.Options{HostLoopback: true})
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	go func() {
		c, err := l.Accept()
		if err == nil {
			io.WriteString(c, "hello from the host\n")
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, s, fullAddr(winnat.GatewayIP.String(), port), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if line != "hello from the host\n" {
		t.Fatalf("got %q", line)
	}

	// Nothing listens there any more: the guest must see a refusal, not a hang.
	l.Close()
	if c, err := gonet.DialContextTCP(ctx, s, fullAddr(winnat.GatewayIP.String(), port), ipv4.ProtocolNumber); err == nil {
		c.Close()
		t.Fatal("connection to a closed host port succeeded")
	}
}

func TestPublishedPort(t *testing.T) {
	nat, s := newGuest(t, winnat.Options{})
	l, err := gonet.ListenTCP(s, tcpip.FullAddress{Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "guest got %s", line)
			}()
		}
	}()

	hostPort, err := nat.Publish(0, 8080)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("guest :8080 published as 127.0.0.1:%d", hostPort)
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	io.WriteString(c, "hi\n")
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "guest got hi\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}
