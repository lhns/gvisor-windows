# 1n: bridge networking and cgroups inside gVisor

Goal: the strict acceptance test (all 10 steps; default bridge network; `-p 8080:80` probed from the runner; compose service names; Podman's default cgroup manager) passes against Podman running inside `runsc` built from this repo, on Linux CI. Starting point: `0c-linux-strict-bar.md` (stock runsc passes no strict configuration).

Result: **all 10 steps pass on Podman 6.2.0 (netavark nftables firewall driver) and on Podman 5.7.1 (netavark iptables driver)**, rootful, `cgroup_manager = cgroupfs`, `runsc --in-sandbox-cgroup=v2`, runsc and `gvisor_sentry` built from this branch.

CI runs (workflow `.github/workflows/podman-acceptance-linux.yml`, job `bridge`, branch `gvisor-bridge-net`):
- final: https://github.com/lhns/gvisor-windows/actions/runs/45876271352 (`--nftables` flag, drop-in config)
- first fully green run: https://github.com/lhns/gvisor-windows/actions/runs/45875558242
- Podman 5.7.1 first green run: https://github.com/lhns/gvisor-windows/actions/runs/45870055340
- `GOOS=windows go build ./runsc/boot/...` (workflow `build`): https://github.com/lhns/gvisor-windows/actions/runs/45876276333 (success, final commit)

## How the job runs

`install.sh` with `BUILD_RUNSC=1` replaces the release binaries with `go build ./runsc` and `./runsc/cmd/sentry` from the checkout (same approach as `test/e2e/syscall-survey/faults.sh`). `nlcheck` (`test/e2e/podman-acceptance/harness/nlcheck`) runs first inside a sandbox and checks the kernel behaviours netavark relies on (netlink options, RTM_SETLINK flags, per-interface sysctls). The engine is `quay.io/podman/stable:v6.2.0` (nft) or `:v5.7.1` (iptables) inside `docker --runtime runsc-fix` / `runsc-nft`, the test runs on the runner over the unix socket exactly as before. Logs of netavark (`RUST_LOG=trace`) and of every `nft` call are collected before step 9 restarts the sandbox.

## Gaps, fixes, source pointers

Each gVisor fix is its own commit with the Linux behaviour it follows in a comment.

| # | Gap (exact symptom) | Cause | Fix | Where |
|---|---|---|---|---|
| 1 | `netavark: host netlink socket: ... Protocol not available (os error 92)` | `setsockopt(SOL_NETLINK, NETLINK_DUMP_STRICT_CHK)` returned ENOPROTOOPT; the other bool options (EXT_ACK, CAP_ACK, ...) did too | accept and store the 0/1 value, read it back with getsockopt (no behaviour change: gVisor sends neither extack TLVs nor strict-filtered dumps) | `pkg/sentry/socket/netlink/socket.go` (`boolOpts`, `SetSockOpt`, `GetSockOpt`) |
| 2 | `Netlink error: Invalid argument (os error 22)` on the first request | `RTM_SETLINK` was rejected when `NLM_F_CREATE` was set (and would have got EEXIST for `NLM_F_EXCL`); netavark sets both to bring `lo` up | RTM_SETLINK ignores CREATE/EXCL/REPLACE, as `rtnl_setlink` does; only RTM_NEWLINK honours them | `pkg/sentry/socket/netlink/route/protocol.go` (`setLink`), `pkg/sentry/socket/netstack/stack.go` (`SetInterface`, `isNew`) |
| 3 | `set sysctl net/ipv4/conf/podman0/route_localnet ... No such file or directory` (also `eth0/arp_notify`, `rp_filter`, `ipv6/.../autoconf`) | `/proc/sys/net/ipv{4,6}/conf` only had `all` and `default` | `conf/<ifname>` is created on lookup for any interface of the task's netns, with the usual knobs as stored values (not acted on by netstack) | `pkg/sentry/fsimpl/proc/tasks_sys.go` (`lazySysDir.Lookup`, `ifaceConfFiles`) |
| 4 | `netavark: failed to set host veth up: Netlink error: No such device (os error 19)` | netavark finds the host veth through `IFLA_LINK` (peer ifindex) of the container end; gVisor never sent it, netavark got index 0 | veth NICs record their peer; RTM_NEWLINK dumps/gets add `IFLA_LINK` (`IFLA_LINK_NETNSID` is not sent) | `pkg/sentry/inet/inet.go` (`Interface.Link`), `pkg/sentry/socket/netstack/stack.go` (`vethPeer`, `ifaceInfo`, `newVeth`), `route/protocol.go` (`writeLinkInfo`) |
| 5 | crun: ``open `/proc/sys/net/ipv4/ping_group_range`: No such file or directory`` | file missing; runc/crun write it for every container | writable stored `"1\t0"` default | `tasks_sys.go` (`pingGroupRange`) |
| 6 | Podman 6.2 netavark 2 only knows nftables (`NETAVARK_FW=iptables` is rejected: `Must provide a valid firewall backend`); `nft` failed with `Protocol not supported` | `NETLINK_NETFILTER` was only registered behind a flag (see gap 11) | enabled with the flag; see gaps 7 to 10 for what the ruleset then needed | `pkg/sentry/socket/netlink/netfilter/protocol.go` |
| 7 | `nft: Could not process rule: Operation not supported` on `meta mark set meta mark \| 0x2000` | `meta mark set` listed as not implemented | implemented (writes `pkt.Mark`) | `pkg/tcpip/nftables/nft_metaset.go` |
| 8 | same error on `meta l4proto { udp, tcp }` and `ct state { established, related }` | `NewSet` refused every set without `NFT_SET_MAP` | key-only sets use the same backend with no data | `pkg/tcpip/nftables/nftables_set.go` (`NewSet`) |
| 9 | `-p 8080:80` reset (netavark batch with `ct mark set ... dnat` failed) | `ct set` was unsupported and `ct mark` had no storage | connection mark on the conntrack entry, `ct mark` get and set | `pkg/tcpip/stack/conntrack.go` (`conn.mark`, `ConnTrackInfo.Mark`), `packet_buffer.go` (`SetConnMark`), `pkg/tcpip/nftables/nft_ct.go` |
| 10 | a second network (compose `acc_default`) failed with the same "Operation not supported" | nft names anonymous sets `__set%d` and references them by id within a batch; gVisor stored the literal name, so re-creating `__set%d` hit "anonymous set already exists" | expand `%d` to the first free number (as `nf_tables_set_alloc_name`), resolve by set id (newest anonymous set with that id wins, ids repeat per nft process) in elements and `lookup` | `pkg/tcpip/nftables/nftables_set.go` (`allocSetName`, `findSet`), `nftables_types.go` (`nftSet.id`), `nft_lookup.go` |
| 11 | nftables flag was `--TESTONLY-nftables` | the nftables package says it is not thread-safe (b/345684870), so it stays opt-in | renamed to a normal flag `--nftables`, still default off | `runsc/config/flags.go`, `config.go` |

Podman 5.7.1 needs none of 6 to 11: netavark 1.x has an iptables driver, and gVisor's iptables (legacy getsockopt ABI: MASQUERADE, DNAT, addrtype, conntrack, comment, multiport) already covers what it emits. That path needed only gaps 1 to 5 plus `iptables-legacy` in the sandbox (the image ships iptables-nft, which wants NETLINK_NETFILTER).

Known, not fixed: `natOp.Dump()` is still unimplemented (`nft list ruleset` prints a `dnat` as `snat`; evaluation is correct); IFLA_LINK_NETNSID; conntrack mark and the new proc files are not in the checkpoint state (`nosave`); the new fields were not added to the generated `*_state_autogen.go` files.

## Cgroups

`/sys/fs/cgroup` was read-only because **Docker puts `ro` into the bundle's OCI spec** for unprivileged containers (inspect: `docker inspect` shows `privileged=false`; the sandbox then lists `cgroup2 ro,nosuid,noexec,nsdelegate`), and gVisor honours it. Not a gVisor defect, so gVisor is unchanged. Harness fix: the engine container is started with `docker run --privileged` (the documented way to run Podman in a container; with it the mount is `rw`, `cgroupns=private`). An in-sandbox `mount -o remount,rw /sys/fs/cgroup` also works (0c), but is no longer needed. With rw, Podman's default `cgroup_manager = "cgroupfs"` and `cgroups = enabled` work as they are; `cgroups=disabled` is gone from the harness. When a spec asks for it, `runsc --in-sandbox-cgroup=v2` is still required (v1 makes Podman 5 exit).

## Changes to Podman's defaults (harness only)

- `/etc/containers/containers.conf` is also installed as a late drop-in `containers.conf.d/99-acceptance.conf` (`log_driver=k8s-file`, `events_logger=file`, `runtime=crun`, `cgroup_manager=cgroupfs`). Podman 6.2's image ships drop-ins that set `log_driver=journald`, which overrides the main file; compose then failed with `using --follow with the journald --log-driver but without the journald --events-backend (file)`.
- Podman 5.7.1: `NETAVARK_FW=iptables` and `dnf install iptables-legacy` with the `iptables*` binaries linked to it. Podman 6.2.0: netavark default (nftables), runsc `--nftables`.
- engine container: `--privileged` (see above), runsc flags otherwise as in 0a/0c (`--platform=systrap --net-raw --in-sandbox-cgroup=v2 --allow-packet-socket-write`).

## Unresolved / for the owner

- Rootful pasta and slirp4netns were not revisited (not needed for the strict bar).
- The nftables implementation is opt-in and documented as not thread-safe; running Podman 6 on it is unsupported until that is addressed. Default-on was not chosen for that reason.
- The nft gaps 7 to 10 and the netlink gaps 1 to 5 are generic gVisor fixes without Go unit tests; they are covered by `nlcheck` (gaps 1 to 3) and by the acceptance job (the rest). Unit tests are a follow-up before lifting them upstream.

Verdict: With runsc built from this branch, the strict acceptance test (default bridge network, `-p` from the host, compose service-name resolution, cgroupfs manager) passes all 10 steps on Podman 6.2.0 and 5.7.1 after eleven small gVisor fixes in netlink, procfs, veth and nftables, while the read-only cgroup mount turned out to be Docker's `ro` and is fixed in the harness with `--privileged`.
