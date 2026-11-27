# 5. Podman on Windows: acceptance through the stock clients

Nodes: "Acceptance test for Windows" (workloads-acceptance-test-windows), "Podman API on a named pipe" (workloads-podman-api-named-pipe), "Probe Podman inside the sandbox". Decision: ADR 0018. Everything ran on `windows-latest` as a standard user (`ci-std`), workflow `.github/workflows/podman-windows.yml`, branch `podman-windows-harness`.

CI: https://github.com/lhns/gvisor-windows/actions/runs/45900262868 (final; earlier runs 45898606296, 45896423266 (cancelled), 45895929286, 45894759817). The reproducer for the main gap: https://github.com/lhns/gvisor-windows/actions/runs/45900320126 (`windows-runsc`, step "Host-folder rename and rmdir").

## What was built
- `tools/windows/podman/start-engine.ps1`: starts, stops or restarts Podman 5.3.1 in a `runsc.exe run` sandbox: root = the erofs image of `quay.io/podman/stable:v5.3.1` (engine settings baked in: containers.conf, storage.conf with the vfs driver, resolv.conf `10.0.2.3`, the machine marker, `gateway.containers.internal` in /etc/hosts), `/var/lib/containers` on a Windows folder (ADR 0007), `-Share` mounted at `/mnt/<drive>/<path>`, `podman system service tcp://0.0.0.0:8888`, `--network=windows-nat`.
- runsc: `--pipe-forward=GUESTPORT:PIPE` (named pipe, current user only, network logons denied, relayed through the NAT's gateway stack, not a loopback port), `--unix-forward=GUESTPORT:PATH` (AF_UNIX socket on the host), `--host-loopback`. The gateway answers Podman's machine API (`POST http://gateway.containers.internal/services/forwarder/expose|unexpose`, libpod/networking_machine.go) at 10.0.2.2:80 and opens or closes the 127.0.0.1 listener (`winnat.Options.GatewayHTTP`, `NAT.Unpublish`); Podman is unmodified, the marker file `/etc/containers/podman-machine` switches the hook on.
- `test/e2e/podman-acceptance/run.ps1`: the PowerShell twin of `run.sh`, same ten steps, strict mode, against the stock Windows `podman.exe` 5.3.1 (podman-remote release zip) and `docker.exe` 27.3.1 plus the compose plugin 2.29.7 (static zips, no install). `tools/windows/podman/ttydrive` runs a command in a ConPTY for step 4.
- Case sensitivity: the bind mounts carry the option `case-sensitive`; the runsc log warns `ignoring unknown mount option "case-sensitive"` (the generic option parser), so whether the backend applied it was not verified.

## Findings that shaped the design
- The stock podman client cannot dial a named pipe: `podman --url npipe:////./pipe/x` fails with `"npipe" is not a supported schema` (5.3.1; `pkg/bindings/connection.go` knows ssh, unix and tcp on main too). It does dial an AF_UNIX socket on Windows (`--url unix:///ci/std/engine/sock/podman.sock`: a path on the current drive; a drive letter does not survive URL parsing). The docker CLI dials the pipe (`DOCKER_HOST=npipe:////./pipe/podman-ci`) and reached the engine in step 8. So the engine is exposed twice: the pipe for docker, the socket (in a folder only the user can enter) for podman.
- Windows path mapping for `-v C:\...:/w`: not reached (step 6 never got past image storage); the mapping `C:\a\b` to `/mnt/c/a/b` is built (`-Share`), what the client sends was not observed.
- The sqlite state database fails on a host-folder mount (`Error: committing transaction: disk I/O error: permission denied`, the same cause as the rename gap below); the image bakes `static_dir = /run/podman-static` (tmpfs) so the engine starts; state then does not survive a restart.

## Per step
| step | result | cause |
|---|---|---|
| 1 engine-reachable | PASS | podman 5.3.1 over the unix socket: version and info |
| 2 pull | FAIL | registry reached through windows-nat (DNS and TLS work, blobs download); extracting a layer fails: `ApplyLayer ... cleaning up after pivot: remove /.pivot_root...: permission denied`, and `rename .tmp-layers.json -> layers.json: permission denied` |
| 3 run-exit-code | FAIL | pull: `committing the finished image ... rename /var/lib/containers/storage/vfs-images/.tmp-images.json... images.json: permission denied` |
| 4 interactive-tty | FAIL | no container (pull); the ConPTY driver itself ran |
| 5 build-apt | FAIL | pull, same rename error |
| 6 bind-mount | FAIL | no container (pull); mapping untested |
| 7 port-publish | FAIL | no container (pull); the gateway forwarder API is untested end to end |
| 8 compose | FAIL | docker CLI and compose reached the engine; pull fails with the rename error |
| 9 restart-persist | FAIL | restart: the engine did not answer within 300 s after the second start (database and layers were already broken; the second start found `incomplete layer ... deleting it`); not isolated |
| 10 isolation | FAIL | pull |

## Gap list
1. **Host-folder rename, rmdir and delete as a standard user (blocks steps 2-10). Owner: fs (`fsimpl/gofer/winfs`, `winhost`).** Reproducer, a bind mount of a Windows folder at `/w`, root in the guest, busybox sh (test `hostfs` in `tools/windows/runsctest`, run by the `windows-runsc` workflow step "Host-folder rename and rmdir", allowed to fail):
   `cd /w; echo a >x; echo b >y; mv x y` gives `mv: can't rename 'x': Permission denied`; `mkdir d; mkdir d/e; rmdir d/e` gives `Permission denied`; `echo c >z; rm z` gives `rm: can't remove 'z': Permission denied`. `mv d d2` of a directory works; creating and writing works. Only replacement of an existing name and removal fail, as the standard user (the fs tests pass as the runner's administrator). Suspect: the POSIX delete disposition or replace-rename needs rights the handles do not request or the standard user does not have on files the Sentry created (DELETE, or FILE_DELETE_CHILD on the parent).
2. **sqlite on a host folder: same cause (item 1).** Verify after the fix, then drop the tmpfs `static_dir` so step 9 can pass.
3. **Podman client cannot dial named pipes. Owner: upstream/podman, documented workaround: `unix:` socket (ADR 0018).**
4. **Restart.** Re-test after item 1: the second `runsc run` over the same storage folder did not answer within 300 s.
5. **Port publishing (step 7, 8) needs the guest's netavark port mapping** (nftables or iptables in the sandbox, see `docs/results/1n-nftables-safety.md`) and the forwarder API of this branch; untested because no container started.
6. **Case-sensitive storage folder:** the `case-sensitive` option was passed but not verified (item 1 first); Linux images contain names that differ only in case.
7. **Mount options** `newinstance`, `ptmxmode`, `mode` (devpts) and `nodev` are logged as unknown (harmless so far).
8. **A guest Unix socket is not forwarded** (the engine listens on a TCP port of the sandbox, as in the Linux harness).

## Outside runsc/
`pkg/tcpip/winnat`: `DialGuest`, `Relay`, `Unpublish`, `Options.GatewayHTTP` (additive); `runsc/boot/network_winnat.go` holds the forwarder API. Windows-net tests passed on this branch.

Verdict: the stock Windows podman client (over an AF_UNIX socket) and docker CLI (over the named pipe) talk to Podman 5.3.1 running in a runsc.exe sandbox as a standard user, and the engine pulls from a registry through windows-nat; steps 2 to 10 fail on one reproducible fs gap, replacing and removing files on a host-folder mount as a standard user.

## Second round (branch podman-windows-2; fs delete-rights fix merged)
CI: engine and acceptance https://github.com/lhns/gvisor-windows/actions/runs/45907008700 (Podman 6.2.0); runsc tests https://github.com/lhns/gvisor-windows/actions/runs/45904481471 (earlier engine runs: 45905538748, 45906081301).

**Podman version.** `quay.io/podman/stable:latest` is 6.2.x; the first round pinned v5.3.1 by choice, not by an old build. The harness now uses **6.2.0** (image `v6.2.0`, podman-remote 6.2.0 zip, netavark 2.1.0, `podman info`: 6.2.0). Podman 6 refuses cgroups v1 (`level=fatal msg="Cgroups v1 not supported"`): the engine spec needs `--in-sandbox-cgroup=v2` and a `cgroup2` mount at `/sys/fs/cgroup`.

**Step 9, the second start (first round).** Isolated from the logs: runsc exited cleanly (the next `runsc run` re-created the pipe and the socket and the engine logged `API service listening` one second after the start). The readiness probe `podman version` then failed for 300 s with `failed to obtain system memory info: getting store info: rename .../vfs-layers/.tmp-layers.json...: permission denied`: the half-written layer store of the first run could not be repaired (`Found incomplete layer ..., deleting it`), the same delete-rights gap, not a runsc problem. No lock or pipe leftover was involved.

**Case sensitivity.** `case-sensitive` now reaches the host-folder backend without the `unknown mount option` warning (`isHostFolderOption`). The test `case` (two names differing only in case on a bind mount, `windows-runsc` step "Host-folder rename, case sensitivity ...") fails as a standard user on the runner because Windows itself refuses: `fsutil file setCaseSensitiveInfo <dir> enable` gives `error: 0x00000005 Access is denied` for the standard user (it worked non-elevated on the dev machine). Podman's CI therefore enables it on the storage folder once as the administrator (`C:\ci\std\engine\storage`; subfolders inherit). Owner: ops/fs (decide whether a setup step with admin rights is acceptable; the check that two names coexist in Podman's storage was not separately run).

**Forwarder API.** Test `forward` (a guest `wget` POSTs `expose` then `unexpose` to `http://10.0.2.2/services/forwarder/...` as Podman does; the host checks that `127.0.0.1:P` opens, serves the guest's httpd, and is refused again after `unexpose`) passes as a standard user (required step of `windows-runsc`). The gateway-side bug found on the way: the first version routed nothing to the handler (`Connection refused`).

**Firewall driver.** Netavark 2.1.0 uses the nftables driver (`Using nftables firewall driver`). It first failed with `nft ... Unable to initialize Netlink socket: Protocol not supported`: the Sentry's nftables netlink protocol is only enabled by `runsc boot` for `--network=none|sandbox`; `runsc.exe` now calls `nftables.EnableNFTables()` for `windows-nat` (and `--nftables`). Netavark then runs (`Adding route`); whether its DNAT rules work is not known yet because no container starts.

**Per step, Podman 6.2.0 (run 45907008700):**
| step | result | cause |
|---|---|---|
| 1 | PASS | |
| 2 pull | PASS | alpine and debian pulled and unpacked on the host-folder storage (the delete-rights fix) |
| 3 run-exit-code | FAIL (exit 126) | container creation: `container create failed (no logs from conmon): conmon bytes "": readObjectStart: expect { or n, but found \x00` |
| 4 interactive-tty | FAIL | same |
| 5 build-apt | FAIL | `mounting an overlay over build context directory ... mount overlay: ... lowerdir=...,upperdir=...,workdir=...,userxattr: invalid argument` (the Sentry's overlayfs rejects the `userxattr` mount option or the upper on this layer) |
| 6 bind-mount | FAIL | same as 3 |
| 7 port-publish | FAIL | same as 3 |
| 8 compose | FAIL | docker CLI and compose reach the engine; container create fails as in 3 |
| 9 restart-persist | FAIL | volume data: the container run fails as in 3 (the second start of the engine now works) |
| 10 isolation | FAIL | as in 3 |

Second-round gap list (new): (a) **conmon/crun cannot create a container in the sandbox** (blocks 3-10): conmon's sync pipe returns no data; needs `crun` debugging with the engine's `--runtime-flag debug` / conmon log, owner Sentry (the Linux validation ran these on a real Linux sandbox); (b) **overlay mount with `userxattr`** (step 5), owner Sentry/fs; (c) case sensitivity needs an admin step on the runner (above). Gaps 3, 4, 5, 7 of the first round are unchanged except that sqlite and storage now work (steps 1 and 2).

Verdict update: with the delete-rights fix, Podman 6.2.0 in `runsc.exe` pulls and stores images on a Windows folder as a standard user through the stock clients, and the next blocker is conmon/crun creating the first container inside the sandbox.

## Third round (branch podman-sentry-gaps): conmon and the overlay mount

CI: strace diagnostics https://github.com/lhns/gvisor-windows/actions/runs/45909192745 (before the fix) and https://github.com/lhns/gvisor-windows/actions/runs/45910105759 (after); reproducer of the remaining platform defect https://github.com/lhns/gvisor-windows/actions/runs/45910710635 (`windows-sentry-guests`, cases `clonesys` and `clonesys_ns`, allowed to fail); host-folder FIFO https://github.com/lhns/gvisor-windows/actions/runs/45910105775 (test `hostfs` passes with the new FIFO lines). The workflow `podman-diag.yml` (script `tools/windows/podman/diag-run.ps1`) starts the engine with `runsc --strace` and tries a container several ways; `start-engine.ps1` takes extra runsc flags from `RUNSC_EXTRA_FLAGS`.

**Defect 1, "conmon sees no data on its sync pipe": two causes, the first fixed.**
1. Cause found with `--strace` (conmon runs with stdout and stderr on `/dev/null`, which is why Podman reports "no logs from conmon"): conmon creates its `ctl` and `winsz` FIFOs in the container's `userdata` folder, which is on the host-folder storage, and `openat(ctl, O_RDONLY|O_NONBLOCK)` returned EPERM. The gofer filesystem refuses to open FIFOs of the host (`disableFifoOpen`, the stock policy, log line `Rejecting attempt to open fifo/pipe from host filesystem: "ctl"`), and a Windows folder holds only the metadata of a FIFO. conmon logged the error to `/dev/null` and exited 1 before writing its pid to the sync pipe. Fix (`pkg/sentry/fsimpl/gofer`: option `emulateFifoOpen`, set by the host-folder mount; `filesystem.go` opens the FIFO as an in-Sentry named pipe held by the inode, like the synthetic FIFOs): conmon now opens `ctl` and `winsz` for reading and writing, creates its attach socket and goes on to run crun. Test: the `hostfs` test of `windows-runsc` (mkfifo, write, read through a host folder) passes. This touches the host-folder owner's package (one option, nine lines in the open path); the owner should review it.
2. **Platform bug (winstub), blocks every container start.** crun then fails with `crun: read status from sync socket: OCI runtime error`. In the strace, crun's `clone(CLONE_NEWUTS|CLONE_NEWIPC|CLONE_NEWPID|CLONE_NEWNET|SIGCHLD)` returns a child that never executes a syscall, and the Sentry log has `Unhandled user fault: addr=0 ip=11145c access=-w- sig=11` for it. The child resumes at `rip == rcx == 0x11145c`, an address in the stub's veneer area (0x110000 to 0x180000): crun calls glibc's `syscall()` function, whose `syscall` instruction is not preceded by `mov $imm,%eax`, so the patcher makes a veneer (`patchReloc`), and after the syscall the guest continues with the displaced instructions inside the veneer (`movabs $post,%rcx`). A child in a new address space (a new stub process) gets its pages patched again by `MapFile` and the veneers allocated again at `as.vcur`, in another order and at other addresses, so the parent's veneer address in the child's registers points at an unwritten page of zero bytes (`add %al,(%rax)` with rax 0 gives the write fault at address 0). `fork()` of glibc is not affected because it uses `mov $56,%eax; syscall` (table entry, same address in every address space through `siteCache`). Reproducer: `test/guests/clonesys.c` (static glibc, `syscall(SYS_clone, SIGCHLD, 0, 0, 0, 0)`, also with `CLONE_NEWPID|NEWUTS|NEWIPC`); under `sentryguest` the child prints nothing and does not exit normally (cases `clonesys`, `clonesys_ns` in `windows-sentry-guests`). Fix proposals for the platform owner: give a veneer an address that depends only on its site (like `siteCache` does for table entries), or copy the parent's veneer area and tables (`vcur`, `restartAt`, `ud2Sites`) to the child in `PostFork`, or translate rip and rcx from a veneer to the child's veneer before the first resume.

**Defect 2, "overlay mount with userxattr rejected": cause found, fix is in the engine spec.** The mount that fails is buildah's overlay over the build context (`mount overlay ... lowerdir=/var/tmp/libpod_builder.../build,upperdir=/var/tmp/buildah-context-.../upper,...,userxattr`), not storage. The Sentry parses `userxattr` correctly; the `invalid argument` comes from `overlay.FilesystemType.GetFilesystem`: for a mount made through `mount(2)` the filesystem of `upperdir` must be `tmpfs` or `9p` (the host-folder and gofer mounts), and `/var/tmp` was a directory of the root, which is an overlay filesystem (`--overlay2=root:memory` over the erofs image). Why Podman asks for `userxattr` was not investigated (the option is harmless: the Sentry then uses `user.overlay.*` xattrs). Fix: `/var/tmp` is a tmpfs in the engine spec (`start-engine.ps1`), as it is on the Linux harness. Not verified end to end: no container starts yet (defect 1.2), so `podman build` could not be run.

**Per step.** Steps 3 to 10 still fail at container creation (crun, defect 1.2); steps 1 and 2 pass as before. The acceptance workflow was not re-run to a result different from the second round, because the first container start needs the platform fix.

Verdict update: the conmon failure was a FIFO on the host folder (fixed in the Sentry), and the next blocker is a platform bug, a `clone` child in a new address space resuming inside the parent's veneer (reproducer `clonesys`), not a Sentry gap.

## Third round (branch podman-windows-3; veneer, conmon FIFOs, /var/tmp tmpfs, case-sensitive folders merged)
CI: https://github.com/lhns/gvisor-windows/actions/runs/45924051831 (Podman 6.2.0, standard user). Earlier runs of the round: 45921171038, 45922552504; reproducer run 45923294271.

| step | result | what happens |
|---|---|---|
| 1 engine-reachable | PASS | |
| 2 pull | PASS | |
| 3 run-exit-code | FAIL (126) | containers are created and started now (crun's clone survives), then `failed to connect to container's attach socket ... dial unixpacket /proc/self/fd/N: connect: connection refused` |
| 4 interactive-tty | FAIL | same attach failure |
| 5 build-apt | FAIL | the RUN step: `setup network: pasta failed ... Unable to parse /proc/sys/net/ipv4/ip_local_port_range / Couldn't switch to pasta namespaces: Operation not permitted`, then `error running container: did not get container start message from parent: EOF` |
| 6 bind-mount | FAIL | attach socket |
| 7 port-publish | **PASS** | `podman run -d -p 8080:80 nginx` through the gateway forwarder API and netavark's nftables DNAT; nginx answers at 127.0.0.1:8080 on Windows |
| 8 compose | FAIL | both services start on the compose network and the cli container exits 0; the check fails because the logs are lost: `[conmon:w]: Failed to chmod stderr` (conmon's fchmod on its log stream) |
| 9 restart-persist | FAIL | volume run needs attach |
| 10 isolation | FAIL | `exec` needs attach |

The required steps (1, 2, 7) are checked by a strict workflow step; the acceptance step itself stays `continue-on-error` only so that the logs are uploaded.

Findings and gaps:
- **Attach socket (blocks 3, 4, 6, 9, 10).** Minimal reproducer `test/guests/unixsock.c` (`runsctest -only sockfs`, in the `windows-runsc` host-folder step, prints only): a `SOCK_SEQPACKET` socket bound and listening on a host-folder bind mount refuses `connect` (`Connection refused`), both by path and through `/proc/self/fd/N` of an O_PATH descriptor; on tmpfs both work. Owner: fs (`fsimpl/gofer`, bound endpoints are lost on host-folder dentries, possibly by the revalidation that became the default). Trying `cache=exclusive` on the storage mount changed nothing (reverted). Putting the container directories (`vfs-containers`) on a tmpfs did not fix Podman's failure either, so there is a second cause not yet isolated: conmon binds `userdata/attach` with a path longer than the 108 bytes of `sun_path` (via `/proc/self/fd`), and podman connects through `/proc/self/fd/N`; this needs the strace of conmon (`podman-diag`'s `--strace` run lost its log: the debug log stayed at 925 bytes, a logging deadlock with `--strace` to be looked at).
- **conmon: `Failed to chmod stderr` (step 8 logs).** fchmod of a std stream (pipe or /dev/null) is refused. Owner: Sentry.
- **pasta (step 5).** Buildah RUN uses pasta: `/proc/sys/net/ipv4/ip_local_port_range` is missing in the Sentry's procfs (reproducer: `cat /proc/sys/net/ipv4/ip_local_port_range` in the guest) and pasta's namespace switch fails with EPERM. Owner: Sentry (procfs sysctl) and netns/setns. Workaround to try: `build --network=host`-style, not applied (strict mode).
- **Podman 6 needs cgroup v2**: `--in-sandbox-cgroup=v2` and a `cgroup2` mount are in the engine spec (round 2).

## Fourth round (branch podman-sentry-gaps-2): attach socket, pasta, Go GC crash
CI: acceptance https://github.com/lhns/gvisor-windows/actions/runs/45928866953 (Podman 6.2.0, standard user); socket reproducer https://github.com/lhns/gvisor-windows/actions/runs/45926068962 (`windows-runsc`, required step); strace diagnostics (`podman-diag`) 45926978293, 45927543429, 45929187209.

| step | result | what happens |
|---|---|---|
| 1 engine-reachable | PASS | |
| 2 pull | PASS | |
| 3 run-exit-code | **PASS** | `podman run --rm alpine echo ok`: the attach socket works |
| 4 interactive-tty | FAIL | the podman client says `The input device is not a TTY` inside the test's ConPTY (`ttydrive`), the typed line never reaches it; the guest side is fine (`TTY=/dev/pts/0`, SIGINT arrives). A harness or Windows-client issue, not looked at further |
| 5 build-apt | FAIL | pasta is gone (below); the RUN step of the debian image now fails with `/bin/sh: error while loading shared libraries: /lib/x86_64-linux-gnu/libc.so.6: file too short` |
| 6 bind-mount | **PASS** | |
| 7 port-publish | PASS | |
| 8 compose | **PASS** | |
| 9 restart-persist | **PASS** | |
| 10 isolation | **PASS** | (one earlier run failed it: `acc-b sees` the other container's file, not reproduced in the last run) |

**1. Unix sockets on host folders (fixed).** `bind(2)` on a host folder created the socket file as metadata only (an ADS record), so the generic `MknodAt` never fell back to its in-memory socket and `connect` found no endpoint (`Connection refused`). `winInode.hostMknod` (`fsimpl/gofer/winfs_windows.go`) now returns EPERM for a socket with an endpoint, which makes `MknodAt` create the synthetic dentry that keeps the endpoint in the Sentry (as for a socket on tmpfs; the file exists in the guest's view only and is gone with the dentry; it shows in directory listings). The reproducer `test/guests/unixsock.c` (`runsctest -only sockfs`, a required step of `windows-runsc` now) binds as conmon does (through `/proc/self/fd/DIRFD/sock`), connects from a forked child directly and through an O_PATH descriptor (`/proc/self/fd/N`, as Podman does) and from the parent, and exchanges data: all steps pass on tmpfs and on a host-folder bind mount.

**2. Second attach-socket failure: not a second cause.** With the fix above `podman run` attaches; the failure Podman showed with its container directory on tmpfs did not reproduce (the runs before the fix all had the attach socket in `userdata` on the host folder). No change to the connect path through `/proc/self/fd` was needed: the child-process and O_PATH cases of `unixsock` pass on tmpfs and host folder.

**3. pasta.**
- `/proc/sys/net/ipv4/ip_local_port_range` existed (netstack's port range) but was printed as `16000 65535` with a space; pasta wants Linux's tab (`Unable to parse`). Fixed in `fsimpl/proc/tasks_sys.go`.
- The `setns` EPERM is not a Sentry bug. The strace shows pasta (started as root: "will change to nobody") calling `capset` to keep CAP_SYS_ADMIN, then `setgid`/`setuid(65534)`, after which the permitted set is empty (Linux clears it too; pasta sets no keepcaps), then `setns(netns, CLONE_NEWNET)` -> EPERM. On Linux runsc and on runc the same flow fails identically (docs/results/0c-linux-strict-bar.md: rootful pasta is refused by Podman 5.7 and fails like this on 6.2). pasta is meant for rootless Podman.
- Why a rootful build asked for pasta: Buildah picks its rootless isolation by default (`--network=bridge` gives `cannot use networks as rootless`), which uses pasta; the cause of that choice inside the sandbox was not found. `--isolation=oci` and `--isolation=chroot` both build. The engine now sets `BUILDAH_ISOLATION=oci` (`start-engine.ps1`), which makes the RUN step of the build get past networking.

**4. Guest Go crash in the AVX-512 GC scan (new, fixed by a workaround).** Twice in one acceptance run the engine (Podman, Go 1.26) died: `SIGSEGV addr=0xfffffffffffffff8` in `internal/runtime/gc/scan.ScanSpanPackedAVX512` (runs 45926978291 and 45927543429, `engine.err`), taking the docker pipe and the engine with it (steps 8, 9 failed after it). The Sentry's CPUID exposed the host's AVX-512; a sporadic fault of an AVX-512 instruction points at vector state lost across a stop (`winstub` saves XSAVE state with the mask of XCR0, the debugger path redirects threads). Not diagnosed in the platform; workaround: for platform `winstub` the guest's CPUID has no AVX512 feature (`cpuid.FeatureSet.UnsetAVX512`, `runsc/boot/loader.go`), so Go does not use the code. With it, 3 acceptance runs in a row had no engine death. For the platform owner: guest code using AVX-512 (Go's GC scan loop, with a GC worker under preemption) sometimes faults at an address of -8, which looks like a lost or stale vector or opmask register after an interrupt or fault stop.

**5. Open: debian container `libc.so.6: file too short` (blocks step 5).** The vfs driver copies the image layer into the container layer with `copy_file_range`, which returns the full size (1926232) for `libc.so.6`; the image layer's file is intact (`wc -c` 1926232, ELF magic). The nested `/bin/sh` of the build container opens `/lib/x86_64-linux-gnu/libc.so.6` through the new layer and `ld.so` closes it again without a `read` in the strace, then prints the error. The diag run 45929187209 has the strace. Likely a host-folder size or cached-pages inconsistency of the copied file (the new layer's file is written through the page cache and read right after by another process); not isolated. Next: a guest that does `copy_file_range` into a host-folder file and reads it from another process (`runsctest`), then compare sizes seen by `fstat` and `read`.

**Also fixed on the way:** the debug log of `runsc.exe run` was closed right after start-up (`defer f.Close()` in `newInstance`), so every message after the first 10 lines was lost; `--debug` and `--strace` logs work again.

Verdict: with in-Sentry sockets on host folders, the tab in `ip_local_port_range`, `BUILDAH_ISOLATION=oci` and AVX-512 hidden from the guest, 8 of the 10 acceptance steps pass through the stock Windows clients; the two left are the terminal input of the test driver and a debian container reading a copied library as truncated.
