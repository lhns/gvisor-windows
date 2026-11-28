package winnat

import (
	"context"
	"errors"
	"net"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// serveDNS answers the queries of one guest flow by resolving through the
// host resolver (on Windows: the system resolver, with its search list, hosts
// file and VPN settings). A, AAAA, CNAME, MX and TXT are answered; other
// types get an empty NOERROR.
func (n *NAT) serveDNS(c net.Conn) {
	defer c.Close()
	buf := make([]byte, 4096)
	for {
		c.SetReadDeadline(time.Now().Add(dnsIdle))
		m, err := c.Read(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:m]...)
		go func() {
			if resp := n.answerDNS(q); resp != nil {
				c.Write(resp)
			}
		}()
	}
}

func (n *NAT) answerDNS(query []byte) []byte {
	if n.opts.UpstreamDNS != "" {
		return n.forwardDNS(query)
	}
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 || qs[0].Class != dnsmessage.ClassINET {
		return nil
	}
	q := qs[0]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	name := q.Name.String()
	host := name[:len(name)-1]

	rcode := dnsmessage.RCodeSuccess
	var answers []dnsmessage.Resource
	add := func(b dnsmessage.ResourceBody) {
		answers = append(answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: q.Name, Type: q.Type, Class: dnsmessage.ClassINET, TTL: 30},
			Body:   b,
		})
	}
	// fail maps a lookup error to a response code. Only A distinguishes
	// "no such name" from "no data of this type", because the host
	// resolver does not.
	fail := func(err error, nxdomain bool) {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			if nxdomain {
				rcode = dnsmessage.RCodeNameError
			}
			return
		}
		rcode = dnsmessage.RCodeServerFailure
	}
	switch q.Type {
	case dnsmessage.TypeA:
		ips, err := n.opts.Resolver.LookupNetIP(ctx, "ip4", host)
		if err != nil {
			fail(err, true)
		}
		for _, ip := range ips {
			if ip.Is4() {
				add(&dnsmessage.AResource{A: ip.As4()})
			}
		}
	case dnsmessage.TypeAAAA:
		ips, err := n.opts.Resolver.LookupNetIP(ctx, "ip6", host)
		if err != nil {
			fail(err, false)
		}
		for _, ip := range ips {
			if ip.Is6() && !ip.Is4In6() {
				add(&dnsmessage.AAAAResource{AAAA: ip.As16()})
			}
		}
	case dnsmessage.TypeCNAME:
		cn, err := n.opts.Resolver.LookupCNAME(ctx, host)
		if err != nil {
			fail(err, true)
		} else if cn != name {
			if cname, err := dnsmessage.NewName(cn); err == nil {
				add(&dnsmessage.CNAMEResource{CNAME: cname})
			}
		}
	case dnsmessage.TypeMX:
		mxs, err := n.opts.Resolver.LookupMX(ctx, host)
		if err != nil {
			fail(err, false)
		}
		for _, mx := range mxs {
			if mxn, err := dnsmessage.NewName(mx.Host); err == nil {
				add(&dnsmessage.MXResource{Pref: mx.Pref, MX: mxn})
			}
		}
	case dnsmessage.TypeTXT:
		txts, err := n.opts.Resolver.LookupTXT(ctx, host)
		if err != nil {
			fail(err, false)
		}
		for _, t := range txts {
			add(&dnsmessage.TXTResource{TXT: []string{t}})
		}
	}

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: h.ID, Response: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: rcode,
	})
	b.EnableCompression()
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
		return nil
	}
	for _, a := range answers {
		var err error
		switch body := a.Body.(type) {
		case *dnsmessage.AResource:
			err = b.AResource(a.Header, *body)
		case *dnsmessage.AAAAResource:
			err = b.AAAAResource(a.Header, *body)
		case *dnsmessage.CNAMEResource:
			err = b.CNAMEResource(a.Header, *body)
		case *dnsmessage.MXResource:
			err = b.MXResource(a.Header, *body)
		case *dnsmessage.TXTResource:
			err = b.TXTResource(a.Header, *body)
		}
		if err != nil {
			return nil
		}
	}
	resp, err := b.Finish()
	if err != nil {
		return nil
	}
	return resp
}

// forwardDNS relays one query to opts.UpstreamDNS over UDP and returns the answer.
func (n *NAT) forwardDNS(query []byte) []byte {
	c, err := n.opts.Dialer.Dial("udp", n.opts.UpstreamDNS)
	if err != nil {
		return nil
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(query); err != nil {
		return nil
	}
	buf := make([]byte, 4096)
	m, err := c.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:m]
}
