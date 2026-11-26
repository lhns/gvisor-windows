# 2. Windows network: a user-space NAT and published ports

Nodes: "Netstack to Windows sockets" (net-netstack-windows-link), "Publish guest ports on localhost" (net-port-forwarding), "Windows CI as a standard user" (ops-windows-ci-standard-user). Decision: ADR 0014.

## Result
A sandbox network mode that needs no admin, no driver and no firewall rule. Tested without a guest: the tests drive a real netstack ("the guest") attached to the NAT. All pass on windows-latest, run as a freshly created standard user:

| Test | What it does | Result |
|---|---|---|
| TestDNS | guest UDP query to 10.0.2.3:53 for github.com A, NXDOMAIN for a `.invalid` name, and Go's own resolver pointed at the virtual DNS | pass |
| TestHTTPS | guest resolves github.com through the NAT, TLS handshake, `GET https://github.com` | 200 OK, 537,108 bytes |
| TestUDPRoundTrip | three datagrams to a host UDP echo server, uppercased and returned | pass |
| TestTCPRefusedAndAccepted | TCP to a host listener, then to a closed port (the guest gets a refusal, not a hang) | pass |
| TestPublishedPort | guest listener on :8080, `Publish(0, 8080)`, a Windows client connects to 127.0.0.1:port | pass |
| TestAdminProbe | tries to create a file in `C:\Windows`; env `GVISOR_ADMIN_PROBE` says whether that must be denied | pass |

CI (workflow `.github/workflows/windows-net.yml`, branch `netstack-windows`): https://github.com/lhns/gvisor-windows/actions/runs/45879246873 (green, final). First run, where everything except the last step's quoting passed: https://github.com/lhns/gvisor-windows/actions/runs/45878899724.

Standard-user proof, from the same run: the network tests ran as `ci-std` (Users group only, `elevated token: false`); writing to `C:\Windows` gave `Access is denied`; running the probe with `allowed` expected as that user fails (exit 1) and the workflow requires that failure; the same `allowed` probe passes as the runner's administrator.

## Design
```
guest app -> Sentry netstack (eth0 10.0.2.15) --pipe--> gateway netstack (promiscuous, spoofing)
                                                           | TCP forwarder: net.Dial, then accept the guest SYN
                                                           | UDP forwarder: one host UDP socket per flow, 60 s idle
                                                           | UDP 10.0.2.3:53: Sentry answers via Windows resolver
127.0.0.1:H (Windows listener) --accept--> gonet dial from the gateway into 10.0.2.15:P
```
- Code: `pkg/tcpip/winnat` (`nat.go` stack and `AttachGuest`, `tcp.go` forwarder, `Publish` and relay, `udp.go`, `dns.go`). Plain Go, no build tags.
- Alternative considered: a link endpoint that parses frames and keeps a NAT table. More code and more bugs than netstack's own forwarders, which already do reassembly, TCP state and ARP.
- DNS answers A, AAAA, CNAME, MX, TXT through `net.DefaultResolver` (system resolver, search list, hosts file). The host resolver cannot tell NXDOMAIN from "no data", so only A returns NXDOMAIN; AAAA, MX and TXT misses are empty NOERROR.
- Wiring in runsc: `--network=windows-nat` (new `NetworkType`) and `--publish=8080:80,0:9000` (host port 0 picks a free one). `runsc/boot/loader.go` creates the stack and calls `setupWindowsNAT` (`runsc/boot/network_winnat.go`) instead of waiting for `CreateLinksAndRoutes`. Annotation: any flag can be set with `dev.gvisor.flag.<name>` where flag overrides are enabled, so `dev.gvisor.flag.publish` works without extra code. Not run: runsc itself cannot start sandboxes on Windows yet, so the mode is compiled (Windows and Linux) and tested through the library.
- Guest resolv.conf: not written by the Sentry. Point it at `nameserver 10.0.2.3` (`winnat.DNSIP`) when preparing the container, as Podman's `--dns` does.

## Flags and API
- `--network=windows-nat`, `--publish=hostPort:guestPort[,...]` (TCP, bound to 127.0.0.1 only).
- Go: `winnat.New(Options{Dialer, Resolver, HostLoopback})`, `(*NAT).AttachGuest(*stack.Stack)`, `(*NAT).Publish(host, guest) (hostPort, err)`, `(*NAT).Close()`.

## Standard-user CI
`tools/windows/ci/run-as-standard-user.ps1` creates a local user in `Users` only (random password), grants it RX on the program and `-GrantPath` folders (M on `-WritablePath`), starts the program with CreateProcessWithLogonW (`ProcessStartInfo` with `UserName`/`Password`, the mechanism of `Start-Process -Credential`), relays stdout and stderr, kills it after `-TimeoutSeconds` (exit 124), removes the user, and exits with the program's exit code. Adopting it in another workflow:
1. Build as the runner's admin (`go test -c -o C:\ci\bin\x.test.exe ./pkg/...`): the standard user cannot read the Go cache or toolchain.
2. `pwsh tools/windows/ci/run-as-standard-user.ps1 -Program C:\ci\bin\x.test.exe -Arguments '-test.v' -Environment @{ K = 'v' } -GrantPath C:\ci\bin`.
3. Pass test flags quoted (`'-test.v'`): pwsh splits an unquoted `-test.v` into two arguments when calling a native program directly.
The script works from a pwsh step (needs `Process.Kill($true)`, PowerShell 7). Scheduled tasks were not needed.

## Not verified / open
- Throughput and latency (two netstacks, one copy each way) are unmeasured.
- A guest process never ran in this setup; the Sentry-side wiring is compile-checked only.
- No ICMP echo to the internet, no IPv6 beyond loopback, no UDP port publishing, no host-to-guest UDP, no TCP DNS (truncated answers do not occur for the supported types).
- `HostLoopback` is Options-only; decide whether a flag is wanted.
- Windows Defender Firewall inbound prompts are avoided by loopback-only listeners; not tested on a desktop where the prompt would appear.

Verdict: a standard user on stock windows-latest can resolve DNS, fetch https://github.com and publish a guest port on 127.0.0.1 through a netstack NAT with no admin rights, and CI proves the account really was unprivileged.
