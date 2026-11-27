// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package main

import (
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/socket/netfilter"
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netlink/route" // AF_NETLINK, NETLINK_ROUTE
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netlink/uevent"
	"gvisor.dev/gvisor/pkg/sentry/socket/netstack" // AF_INET, AF_INET6, AF_PACKET providers.
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/tcpip/winnat"
)

// newNetNamespace gives the guest a netstack with lo and eth0 behind the
// user-space NAT of the windows-nat network mode (the same stack runsc builds
// for --network=windows-nat), so socket tests have a network.
func newNetNamespace(tk *kernel.Timekeeper, userns *auth.UserNamespace) *inet.Namespace {
	s := netstack.NewStack(stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
		Clock:              tk,
		Stats:              netstack.Metrics,
		HandleLocal:        true,
		RawFactory:         raw.EndpointFactory{},
		DefaultIPTables:    netfilter.DefaultLinuxTables,
	}), 1)
	sack := tcpip.TCPSACKEnabled(true)
	tcpMust(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &sack), "tcp sack")
	ttl := tcpip.DefaultTTLOption(netstack.DefaultTTL)
	tcpMust(s.Stack.SetNetworkProtocolOption(ipv4.ProtocolNumber, &ttl), "ipv4 ttl")
	tcpMust(s.Stack.SetNetworkProtocolOption(ipv6.ProtocolNumber, &ttl), "ipv6 ttl")
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	tcpMust(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &moderate), "tcp receive buffer tuning")
	delayed := tcpip.TCPDelayedACKEnabled(false)
	tcpMust(s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &delayed), "tcp delayed ack")
	nat, err := winnat.New(winnat.Options{})
	must(err, "creating the NAT")
	must(nat.AttachGuest(s.Stack), "attaching the guest to the NAT")
	return inet.NewRootNamespace(s, nil, userns)
}

func tcpMust(e tcpip.Error, what string) {
	if e != nil {
		fail("%s: %v", what, e)
	}
}
