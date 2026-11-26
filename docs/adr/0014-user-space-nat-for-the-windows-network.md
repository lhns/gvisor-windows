# 0014. Windows network: the guest's packets end in a user-space NAT

- Status: Accepted
- Date: 2026-11-26

## Context
hostinet needs Linux host sockets and fdbased needs a TAP or packet socket; both are stubbed on Windows (1b), and a TAP/WinTun driver needs admin. The goal is a container network that works as a standard user and publishes ports on localhost.

## Decision
- `--network=windows-nat` gives the Sentry's netstack an eth0 (10.0.2.15/24, gateway 10.0.2.2, DNS 10.0.2.3) whose other end is a second, small netstack (the gateway) in the same process, joined by `link/pipe` with Ethernet framing.
- The gateway runs promiscuous with spoofing and netstack's TCP and UDP forwarders: each guest flow is re-originated as an ordinary Windows `net.Dial` (the host connection is made first, so a refusal becomes an RST). This is gvisor-tap-vsock's model.
- DNS to 10.0.2.3:53 is answered by the Sentry through the Windows resolver (`net.DefaultResolver`), not forwarded as packets.
- Published ports (`--publish host:guest`, also settable by the `dev.gvisor.flag.publish` annotation where flag overrides are allowed) are `127.0.0.1` listeners that dial the guest through the gateway stack. Loopback only, TCP only.
- The guest reaches host loopback services only with `Options.HostLoopback` (10.0.2.2 maps to 127.0.0.1); off by default and not exposed as a flag.
- The code is plain Go in `pkg/tcpip/winnat` with no build tags, so the Linux build compiles it too.

## Consequences
- No driver, no admin, no firewall rule; inbound listeners bind loopback only.
- ICMP echo to the internet, IPv6 beyond loopback, multicast and raw sockets do not leave the sandbox; guest-to-host traffic is limited to what `net.Dial` can do.
- Every packet crosses two netstacks (copy cost); measure before optimising.
- Source addresses seen by remote hosts are the Windows host's; the guest's own IP is not visible outside.
