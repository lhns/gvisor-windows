# Phase 0c: strict bar on stock gVisor (Linux)

Question: what does stock `runsc` need for the strict bar of ADR 0002, i.e. `-p 8080:80` reachable from outside the sandbox and compose with two services reaching each other by service name? gVisor itself was not modified.

CI runs (workflow `.github/workflows/podman-acceptance-linux.yml`, branch `linux-strict-bar`, ubuntu-latest x86_64; raw artifacts attached to each):
- matrix (`strict-*` jobs): https://github.com/lhns/gvisor-windows/actions/runs/45859911118 (identical results in 45855018179)
- nftables flag, netavark sockopt shim, slirp4netns install: https://github.com/lhns/gvisor-windows/actions/runs/45860718417
- cgroupfs (rw remount) + host net: https://github.com/lhns/gvisor-windows/actions/runs/45862047268
- netavark trace that pins the first rtnetlink gap: https://github.com/lhns/gvisor-windows/actions/runs/45864049471
- raw rtnetlink/netfilter probe with `ip`/`nft`/`iptables` and the runc control: https://github.com/lhns/gvisor-windows/actions/runs/45856214171

## Versions and flags

- runsc `release-20261123.1`, `--platform=systrap --net-raw --allow-packet-socket-write --in-sandbox-cgroup=v2`, docker `--runtime runsc`, default sandbox network (netstack, not hostinet). The gVisor sources cited below are this repo at `73e44bbdb`; the CI binary is the upstream release, so line numbers may drift from what ran (the behaviour matched in every check).
- Engine: `quay.io/podman/stable:v5.7.1` (Podman 5.7.1) and `quay.io/podman/stable:v6.2.0` (Podman 6.2.0, the newest 6.x tag on quay at the time), rootful, crun, store on a host directory (kernel overlayfs), `--cap-add all --security-opt seccomp=unconfined --user root`. Client on the runner: apt Podman 5.7.0 plus the docker CLI for compose.
- Harness knobs added in `engine-runsc.sh`: `PODMAN_IMAGE`, `CGROUPS` (disabled | enabled = cgroupfs manager), and probe-only `REMOUNT_CG`, `INSTALL_SLIRP`, `SHIM`. Strict mode is `HOSTNET` unset in `run.sh`: step 7 is `-p 8080:80` probed with curl on the runner (docker publishes the sandbox port 8080), step 8 is compose with the default network and `wget http://web:80`. The step list is unchanged. Step 8 always uses the compose default network, i.e. netavark bridge, whatever `--network` the other steps use: no network mode sidesteps the bridge gap for the strict step 8.

## Matrix

Failures give the exact error. Results were identical for Podman 5.7.1 and 6.2.0 unless stated, and every cell was run at least twice with the same outcome.

| cgroups | network | passing steps | failing steps and exact error |
|---|---|---|---|
| disabled | host (HOSTNET variant) | 1-10 all | none |
| disabled | bridge (netavark) | 1, 2, 5 | 3, 4, 6, 10: `netavark: host netlink socket: IO error: Protocol not available (os error 92)` (exit 126); 7: curl `Recv failure: Connection reset by peer` (no container, nothing listens behind docker's published port); 8: compose up fails (same netavark error); 9: fails because the volume run (step 3 style) fails |
| disabled | pasta, 5.7.1 | 1, 2, 5 | `invalid config provided: pasta networking is only supported for rootless mode or when inside a nested userns` (Podman refuses; not a gVisor result) |
| disabled | pasta, 6.2.0 | 1, 2, 5 | `pasta failed with exit code 1: Started as root, will change to nobody. ... Couldn't switch to pasta namespaces: Operation not permitted` (strace: `setns(netns, CLONE_NEWNET)` returns EPERM after pasta did `setuid(65534)` and dropped all caps). A runc `--privileged` control with the same image rejects rootful pasta with the Podman message above, so this is inconclusive for gVisor |
| disabled | slirp4netns, 5.7.1 | 1, 2, 5 | image has no binary: `could not find slirp4netns`; after `dnf install slirp4netns`: `slirp4netns failed: "cannot set device up: Operation not supported"` (likely the tap device; not traced) |
| disabled | slirp4netns, 6.2.0 | 1, 2, 5 | `invalid network "slirp4netns"` (removed upstream; a Podman fact) |
| enabled (cgroupfs) | host | 1, 2, 5 | 3, 4, 6, 7, 8, 9, 10: `crun: enable controllers +cpu +memory +pids +cpuset: open /sys/fs/cgroup/cgroup.subtree_control for writing: Read-only file system` |
| enabled (cgroupfs), `/sys/fs/cgroup` remounted rw first | host | 1-10 all | none. The sandbox mounts cgroup2 `ro,nosuid,noexec,nsdelegate`; `mount -o remount,rw /sys/fs/cgroup` inside the sandbox succeeds and is enough |
| enabled (cgroupfs) | bridge / pasta / slirp4netns | 1, 2, 5 | same network errors as with cgroups disabled |

Native podman on the runner passes all steps (`acceptance-native` job), so the test itself is sound.

Minimal working combination for the strict bar: none found. The nearest working configuration is host networking (1-10 pass with cgroups disabled, or with cgroupfs after the rw remount), which exercises neither `-p` nor service-name DNS.

## The bridge gap, precisely

Probes: runsc `--strace` with podman run directly in the sandbox (run 45856214171), then netavark with `RUST_LOG=trace` (run 45864049471).

1. The earlier diagnosis "netlink socket type gVisor doesn't offer" was wrong. `socket(AF_NETLINK, SOCK_DGRAM, NETLINK_ROUTE)` succeeds. The failing call is the next one: `setsockopt(fd, SOL_NETLINK, NETLINK_DUMP_STRICT_CHK, 1)` returns `ENOPROTOOPT` (errno 92) and netavark treats it as fatal (pasta issues the same call and ignores the failure). Source: `pkg/sentry/socket/netlink/socket.go` `SetSockOpt` (around lines 744-757) lists `NETLINK_BROADCAST_ERROR, CAP_ACK, DUMP_STRICT_CHK, EXT_ACK, LISTEN_ALL_NSID, NO_ENOBUFS, PKTINFO` under "Not supported" and falls through to `ErrProtocolNotAvailable`. The getsockopt side (around line 597) already accepts them. Fix size: accept and store the flags.
2. `NETLINK_NETFILTER` is not offered by default: `nft` and `iptables` (nft backend) fail with `Unable to initialize Netlink socket: Protocol not supported`. The provider exists (`pkg/sentry/socket/netlink/netfilter/protocol.go`, linked in via `runsc/boot/loader.go`) but `NewProtocol` returns `ErrProtocolNotSupported` unless `nftables.IsNFTablesEnabled()`, which is the runsc flag `--TESTONLY-nftables` (`runsc/config/flags.go`). Netavark picks the nftables firewall driver (seen in its trace). Whether the nftables implementation covers what netavark emits (DNAT for `-p`, masquerade, forward chains) is untested.
3. Behind gap 1 (a probe-only LD_PRELOAD shim on netavark that turns that ENOPROTOOPT into success; not a fix) the next failure is `netavark: Netlink error: Invalid argument (os error 22)`. The netavark trace shows its first request: `RTM_SETLINK` (type 19) on index 1 (lo), `IFF_UP`, change mask `IFF_UP`, no attributes, flags `NLM_F_REQUEST|NLM_F_ACK|NLM_F_EXCL|NLM_F_CREATE` (1541), and the ACK carries `-22`. In `pkg/sentry/socket/netlink/route/protocol.go` `setLink` (line ~191) returns `ErrInvalidArgument` when `NLM_F_CREATE` is set, and `netstack.Stack.SetInterface` (`pkg/sentry/socket/netstack/stack.go`, line ~246) would then answer `EEXIST` for `NLM_F_EXCL`. Linux ignores both flags for RTM_SETLINK. Same result with `--TESTONLY-nftables`, with Podman 5.7.1 and 6.2.0, with cgroups disabled or cgroupfs.
4. Unknown: everything after gap 3. Netavark never gets further, so bridge/veth creation with netavark's exact attributes, moving the peer into the container netns, address and route setup, the nftables rules for `-p`, and aardvark-dns reachability are unverified. Plain `ip` in the sandbox (inference only, not netavark's messages) reported success for `ip link add type bridge`, `ip link add type veth peer`, `ip netns add`, `ip link set ... netns`, addresses and routes, and the source has bridge and veth creation (`netstack/stack.go` `newBridge`, `newVeth`), so the real gap may be small.
5. `SetInterface` accepts only IFNAME, MASTER, LINKINFO, ADDRESS, MTU, NET_NS_FD and TXQLEN and answers `ErrNotSupported` (with an "unexpected attribute" warning) for other link attributes (stack.go lines ~214-236). Netavark may send others later; the probes never got that far.

For `-p`: with the bridge broken there is nothing to test the DNAT path end to end. Step 7 sees `Connection reset` in every failing cell because docker's published sandbox port 8080 has no listener.

## Other findings

- `--in-sandbox-cgroup=v2` alone is not enough for `cgroups=enabled`: the cgroup2 mount is read-only, which breaks crun's controller delegation. A rw remount as root inside the sandbox before `podman system service` fixes it. Whether runsc or docker sets the `ro` was not checked.
- Rootful Podman refuses pasta outside a nested userns, so pasta needs a rootless Podman inside the sandbox (not tried).
- The shim, `RUST_LOG` and `--TESTONLY-nftables` are probe tools only.

## Unresolved

- Everything behind the SETLINK flag check: needs a patched runsc.
- Whether `--TESTONLY-nftables` implements the rules netavark's nftables driver emits, and whether `-p` works from the host through docker's published port into the sandbox.
- Cause of slirp4netns "cannot set device up"; pasta `setns` EPERM under a valid rootless setup.

Verdict: Stock runsc passes the strict bar in no tested configuration, because netavark (Podman 5.7.1 and 6.2.0) dies at `setsockopt(NETLINK_DUMP_STRICT_CHK)` (`ENOPROTOOPT`, `netlink/socket.go`) and, once that is bypassed, at `RTM_SETLINK` with `NLM_F_CREATE` (`EINVAL`, `netlink/route/protocol.go` `setLink`), with `NETLINK_NETFILTER` further hidden behind `--TESTONLY-nftables`; only host networking passes all 10 steps.
