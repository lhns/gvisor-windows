# 0e: Podman engine in a rootless runsc on Linux (spike)

Verdict: feasible with 1 known fix (host networking works today, 9 of 10 steps pass); the isolated-network mode needed for the strict bar (-p publish, compose by service name) is not yet working because `--network=sandbox` is refused with `--rootless` and the user-space NAT (`--network=windows-nat`) is killed by seccomp on Linux.

Branch `linux-rootless-spike`, job `spike-rootless` in `.github/workflows/podman-acceptance-linux.yml` (dispatch only, input `spike_rootless`; other jobs are skipped when it is true). Runs: 46091596698 (engine start, first NAT result), 46092181445 (host results below). ubuntu-latest (24.04), runsc built from this tree (`go build ./runsc`, `./runsc/cmd/sentry`, sidecars from the latest release bundle), Podman engine `quay.io/podman/stable:v6.2.0`, client podman 4.9 (apt) and docker compose from the runner, all run as user `spike` (useradd, no sudo, no docker group).

## What the existing Linux acceptance tests use

- runsc: the `acceptance`, `strict`, `netlink-probe`, `bench`, `storage` jobs install the upstream release (`storage.googleapis.com/gvisor/releases/release/latest`, sha512 checked) via `harness/install.sh`. Only the `bridge` job (`BUILD_RUNSC=1`) replaces runsc and `gvisor_sentry` with a build of this branch (docker runtimes `runsc-fix`, `runsc-nft`).
- Steps: `run.sh` runs all 10 steps in every job. `bridge` runs them strict (default bridge, `-p 8080:80`, compose by service name), as 1-8 then 9-10 because step 9 restarts the sandbox. `acceptance` has hostnet variants (`--network=host`, nginx on 8080) and a native-podman control.
- Privileges: everything goes through the root docker daemon: `docker run --runtime runsc --cap-add all --security-opt seccomp=unconfined --user root` (`bridge`: `--privileged`), runsc flags `--net-raw --allow-packet-socket-write --in-sandbox-cgroup=v2`, `sudo` for the host storage dir. Podman inside is rootful (uid 0 in the sandbox). None of it tests rootless runsc.

## Setup of the spike

`harness/rootless-spike.sh` (run as `spike`): OCI bundle with the unpacked engine image (docker used once, as the runner, to export it; unpacked by `spike` with `tar --no-same-owner`), the capability set and mounts of `tools/windows/podman/start-engine.ps1`, store on a host dir in `~spike/work/storage`, `runsc --rootless --ignore-cgroups --platform=systrap --overlay2=root:memory --in-sandbox-cgroup=v2 run`, `podman system service tcp://0.0.0.0:8888` in the sandbox, `socat` turns it into `unix:///home/spike/podman.sock`. The client and `run.sh` run as `spike` against that socket.

Environment needs: `kernel.apparmor_restrict_unprivileged_userns=0` on Ubuntu 24.04 (root, once, as for any rootless runsc/podman); `/etc/resolv.conf` of the rootfs must be the host's for host networking; a clean env (`env -i HOME=...`) because the runner's `XDG_*` variables leaked through `sudo -u`.

## Results

| Mode | Steps passed | Notes |
|---|---|---|
| `--network=host`, `STORAGE_DRIVER=overlay` | 9/10 | native kernel overlay inside the sandbox, `Not using native diff` is logged (xattr opaque probe fails on the host-dir store, see below) |
| `--network=host`, `STORAGE_DRIVER=vfs` | 9/10 | same |
| `--network=windows-nat` (user-space NAT) | 1/10 | engine starts, API reachable through the published port; sandbox dies on the first outbound traffic (below) |
| `--network=sandbox` | 0/10 | runsc refuses to start |

Host mode, per step: 1 engine-reachable, 2 pull, 3 run-exit-code, 4 interactive-tty, 5 build-apt, 6 bind-mount, 7 port-publish (hostnet variant: nginx on 8080), 9 restart-persist, 10 isolation PASS. 8 compose FAIL.

## Blockers

1. `--network=sandbox` with `--rootless`: `sandbox network isn't supported with --rootless, use --network=none or --network=host` (upstream restriction: a rootless sandbox cannot configure a netns). Not worth fixing; the NAT mode is the way around it.
2. `--network=windows-nat` on Linux, three problems, two fixed here:
   - fixed: `runsc/sandbox/sandbox.go` put the sandbox in a fresh netns, so the NAT's published host listener (127.0.0.1:8888) was invisible to the host. `windows-nat` now stays in the starting netns like `host`.
   - fixed: `runsc/boot/loader.go` seccomp filter had no host-socket syscalls for the NAT's `net.Dial/Listen`; `windows-nat` now gets the hostnet filter set.
   - open: after both fixes the sandbox is still killed by seccomp (`runsc` exit status 159 = SIGSYS) during the first `podman pull` (first outbound dial through the NAT, i.e. the sentry process calling the host network). The missing syscall or argument is not identified: no audit record on the runner, and `strace -f` around `runsc run` makes the sandbox exit at start (`containerManager.StartRoot ... connection reset by peer`). Suspects: DNS/`/etc` file access from the Go resolver in the chrooted sentry, a socket option or `socket()` argument outside `hostinet.SockOpts`/`AllowedSocketTypes`, or a syscall the Go `net` package adds (e.g. `getsockopt`/`setsockopt` for keepalive). Next step: compile a probe that runs the NAT dial in the same filter, or run the sentry with the filter in `SECCOMP_RET_LOG` mode.
3. Step 8 (compose) with host networking: `nginx: bind() to 0.0.0.0:80 failed (13: Permission denied)`. An unprivileged host user cannot bind port 80 (`net.ipv4.ip_unprivileged_port_start=1024`); the harness' host-net variant of compose assumes port 80. Not a runsc bug. With host networking there is also no service-name resolution and no network isolation between the engine, its containers and the host (containers see and can bind host ports). It would pass with `ip_unprivileged_port_start=0` or an nginx on a high port.
4. Not blockers but noted: `Not using native diff for overlay ... failed to set opaque flag on middle layer: lsetxattr ... operation not supported` on the host-dir store (rootless gofer on directfs refuses `trusted.*` xattrs; only affects image build speed, step 5 passes), `sysfs`/cgroup mount option warnings, step 4/5 pass without changes.

No Sentry fixes were needed for the host mode: the shared fixes from the Windows work (netlink options, nftables default, pipefs fchmod, inode 0xEFFFFFFD, overlay diff without forced userxattr, ...) plus the engine's `crun` and `overlay` driver were enough for 9 of 10 steps.

## Estimate for a Linux bundle in podman-portable

- Host-network bundle (runsc + sidecars + erofs/OCI rootfs + this launcher): works now, needs packaging, a documented prerequisite for AppArmor userns on Ubuntu 24.04+, a resolv.conf choice, and a decision on what to say about missing network isolation and port 80: about 2 to 3 days.
- Same promise as on Windows (isolated bridge network, `-p`, compose by name) through the user-space NAT: depends on finding the one seccomp gap (probably a few hours to 1 day), then re-running the strict steps 7 and 8 and the netavark/nft path on top of it (the NAT mode on Windows already passes them, so no new Sentry work is expected): about 3 to 5 days in total, with the seccomp gap as the risk.
