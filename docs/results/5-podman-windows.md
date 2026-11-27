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

## Fourth round (branch podman-windows-4)
CI: https://github.com/lhns/gvisor-windows/actions/runs/45926399186 (acceptance); https://github.com/lhns/gvisor-windows/actions/runs/45925916828 (runsc tests).

- **fchmod on a standard stream (conmon `Failed to chmod stderr`): fixed.** The Sentry's pipefs refused every mode change on a pipe with EPERM (it only accepted uid and gid); on Linux fchmod on a pipe works. The inode now keeps its mode (`S_IFIFO` plus the permission bits, `pkg/sentry/fsimpl/pipefs`). Test `fchmod` (`chmod 0777 /proc/self/fd/1` in a guest whose stdout is such a pipe) is a required step; the warning is gone from the engine log.
- **The debug log that stopped after nine lines: fixed, and it was our bug, not strace's.** The refactor that split `run` into `newInstance` (round 3b) left `defer f.Close()` of the debug log in `newInstance`, so every log of a Windows sandbox stopped as soon as the Loader was created (`write ...: file already closed`). The file is now closed with the sandbox, and a failing log write is reported once on stderr (`reportingWriter`). The engine's debug log is 1.4 MB for the acceptance run. The `strace` test (`--strace` on a hello, log must contain `write(` and `exit_group`) guards it.
- **Acceptance steps unchanged:** 1, 2 and 7 pass (required), 3, 4, 6, 8, 9 and 10 fail on the attach socket (host-folder sockets and the second cause, with the Sentry worker), 5 on pasta. Step 8 now has containers that start and exit (`cli-1 exited with code 0`) without the conmon warning; the compose check still lacks the output because logs and attach need the attach socket.
- The full debug log now shows only `rseq`, `sys_442` and `sys_458` as unsupported syscalls during the run; none of them explains the attach failure.

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

**4. Guest Go crash in the AVX-512 GC scan (new, fixed by a workaround).** Twice in one acceptance run the engine (Podman, Go 1.26) died: `SIGSEGV addr=0xfffffffffffffff8` in `internal/runtime/gc/scan.ScanSpanPackedAVX512` (runs 45926978291 and 45927543429, `engine.err`), taking the docker pipe and the engine with it (steps 8, 9 failed after it). The Sentry's CPUID exposed the host's AVX-512; a sporadic fault of an AVX-512 instruction points at vector state lost across a stop (`winstub` saves XSAVE state with the mask of XCR0, the debugger path redirects threads). Not diagnosed in the platform; workaround: for platform `winstub` the guest's CPUID has no AVX512 feature (`cpuid.FeatureSet.UnsetAVX512`, `runsc/boot/loader.go`), so Go does not use the code. The acceptance run with it (45928866953) had no engine death; one run is not proof, the crash was intermittent (the run before it, without the change, passed 8 and 9 too). For the platform owner: guest code using AVX-512 (Go's GC scan loop, with a GC worker under preemption) sometimes faults at an address of -8, which looks like a lost or stale vector or opmask register after an interrupt or fault stop.

**5. Open: debian container `libc.so.6: file too short` (blocks step 5).** The vfs driver copies the image layer into the container layer with `copy_file_range`, which returns the full size (1926232) for `libc.so.6`; the image layer's file is intact (`wc -c` 1926232, ELF magic). The nested `/bin/sh` of the build container opens `/lib/x86_64-linux-gnu/libc.so.6` through the new layer and `ld.so` closes it again without a `read` in the strace, then prints the error. The diag run 45929187209 has the strace. Likely a host-folder size or cached-pages inconsistency of the copied file (the new layer's file is written through the page cache and read right after by another process); not isolated. Next: a guest that does `copy_file_range` into a host-folder file and reads it from another process (`runsctest`), then compare sizes seen by `fstat` and `read`.

**Also fixed on the way:** the debug log of `runsc.exe run` was closed right after start-up (`defer f.Close()` in `newInstance`), so every message after the first 10 lines was lost; `--debug` and `--strace` logs work again.

Verdict: with in-Sentry sockets on host folders, the tab in `ip_local_port_range`, `BUILDAH_ISOLATION=oci` and AVX-512 hidden from the guest, 8 of the 10 acceptance steps pass through the stock Windows clients; the two left are the terminal input of the test driver and a debian container reading a copied library as truncated.

## Fifth round (branch podman-tty): the interactive TTY, and flakiness of the whole acceptance
Runs: tty https://github.com/lhns/gvisor-windows/actions/runs/45931744445; eight parallel repetitions https://github.com/lhns/gvisor-windows/actions/runs/45934083189 (workflow input `runs`); an earlier set of five 45933195487.

**Step 4 (interactive TTY) passes.** The stock Windows podman client printed "The input device is not a TTY" in ttydrive's ConPTY because its standard handles were not consoles: a probe (`pwsh` with `[Console]::IsInputRedirected`) in the ConPTY gave `True,True,True`. ttydrive had no console and redirected std handles of its own (it runs under `Start-Process -RedirectStandardOutput`), and the child created with `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE` and `STARTF_USESTDHANDLES` unset got those, not the pseudoconsole's. Setting `STARTF_USESTDHANDLES` with NULL handles in the STARTUPINFOEX makes the system give the child the pseudoconsole's handles (probe: `False,False,False`); the client then detects the TTY: `TTY=/dev/pts/0`, typed `hello` arrives, Ctrl-C reaches the container (`GOT_INT`), exit code 7. Nothing was needed on runsc's side for step 4 (runsc's own console code opens `CONIN$`/`CONOUT$` and never relied on the std handles).

**Flakiness, 8 repetitions (acceptance as a standard user, Podman 6.2.0), per step: passes out of 8.** Step 5 is still the known failure (libc too short after copy_file_range, Sentry worker).
| step | passes of 8 | failures seen |
|---|---|---|
| 1 engine-reachable | 8/8 (run 7 never reported) | |
| 2 pull | 8/8 (run 7) | |
| 3 run-exit-code | 6/8 | run 8: `writing runtime spec JSON ... userdata/config.json: no such file or directory` (a file that was just created is missing in the tmpfs container directory); in run 1 the engine had died (below) |
| 4 interactive-tty | 7/7 reported | |
| 6 bind-mount | 7/7 reported | |
| 7 port-publish | 6/7 reported | run 1: engine died |
| 8 compose | 6/7 reported | run 1: engine died |
| 9 restart-persist | 6/7 reported | run 1: engine died |
| 10 isolation | 6/7 reported | run 3: `crun: open /dev/null: No such file or directory` in a container, then `read unixpacket ...attach: connection reset by peer` |
(8 jobs, one hung, 7 reported; the earlier set of five had 2 engine deaths and 1 isolation failure.)

Causes of the flaky failures (all Sentry or platform, not runsc or the harness):
1. **The engine sandbox dies mid-run (about 1 in 4 runs; steps 7-9 then fail with "cannot connect").** The one captured crash (the harness now keeps one stderr file per engine start): a Go GC worker of podman itself, `SIGSEGV addr=0xfffffffffffffff8` in `internal/runtime/gc/scan.ScanSpanPackedAVX512` (the guest Go runtime, greenteagc, takes its AVX-512 path), which kills the root container and with it `runsc.exe`. So the AVX-512 hiding is not complete for this Go version (the guest still sees AVX-512 through `cpuid` leaf 7 or `XCR0`), or the vector state is lost across a stop. Owner: platform (winstub / cpuid). Reproducer: any Go program of the same Go version that allocates enough to run the GC in a loop in the guest.
2. **`/dev/null` missing in a container for a short time** (run 3 step 10; also seen as `container create failed (no logs from conmon)`): a race in setting up the container's `/dev` (tmpfs plus device nodes). Owner: Sentry.
3. **A just-created file missing** (run 8 step 3, config.json in the tmpfs container directory): owner Sentry (tmpfs or the host-folder layer under it).
4. **A hung job (1 of 8): no step output, no artifact; it ran past 35 minutes** (an acceptance normally takes 15-20 minutes). Not diagnosed: the job's logs were lost when the run was cancelled; the next hang needs a step timeout inside `run.ps1` (each native call has one, 900 s) and the engine logs.

The required-step list of the workflow is now 1, 2, 3, 4, 6, 7, 8, 9, 10 (5 once the Sentry fix lands) and the acceptance job takes a `runs` input to repeat itself in parallel.

## Sixth round (branch podman-rewrite): native against rewrite mode, 8 runs each
Run https://github.com/lhns/gvisor-windows/actions/runs/45937432174 (`modes=native,rewrite`, `runs=8`, all on windows-latest, standard user, Podman 6.2.0). `native` = `WINSTUB_FSMODE=native`; `rewrite` = `WINSTUB_FSMODE=rewrite` and `WINSTUB_KEEP_AVX512=1` (AVX-512 not hidden). Each job is limited to 40 minutes inside the job, so a hang ends itself and the logs are uploaded (they were, for all 16 jobs). Step timings are in `steps.txt` (`[Ns]`).

| | native (8 runs) | rewrite (8 runs) |
|---|---|---|
| sandbox deaths (SIGSEGV or `fatal error` in the engine's stderr) | 0 | 0 |
| runs that completed all 10 steps | 5 (runs 1-5) | 0 |
| runs that hung or never got going | 3 (runs 6, 7, 8) | 8 |
| step 1 engine-reachable | 7 pass (2 s) | 3 pass (29-52 s), 5 timeouts of 900 s |
| step 2 pull | 5 pass (31-46 s) | 2 pass (99 s in one run), 6 timeouts |
| steps 3, 4, 6, 7, 8, 10 | pass in the 5 complete runs | reached only in rewrite run 5 (3 pass 17 s, 4 pass 25 s, 6 hung 1800 s) |
| step 5 | fails (libc too short, known) | |
| step 9 restart-persist | pass 3 of 5, fail 2 of 5 | |

Native timings in the five complete runs (seconds): step 1 1-2, 2 31-46, 3 7-12, 4 20, 5 31-43 (failing), 6 12-14, 7 71-114, 8 22-26, 9 16-17, 10 10-11; the acceptance takes about 4-5 minutes of test time.

What the logs say:
- **No AVX-512 death in either mode.** The `ScanSpanPackedAVX512` crash did not reproduce in these 16 jobs (it killed the engine in about 1 of 4 runs before; with native mode in this run 0 of 8). The sample is too small to say it is gone.
- **Rewrite mode is much slower and hangs.** With rewrite the engine answers `version` after 29-52 s instead of 2 s, `info` after 28 s, and then stops answering (the engine log ends with a `_ping`; the debug log only shows the watchdog loop with 10 tasks and `Time: Adjusting syscall overhead` samples, no fault, no panic). Rewrite never completed a run. Owner: platform (winstub rewrite mode).
- **Native mode hangs in `vfork` (2 of 8 runs, runs 7 and 8).** The last log line of both is `Entering internal stop (*kernel.vforkStop)`: a task that called `vfork` (or `clone` with CLONE_VFORK) is stopped and never released; run 8 hung at the start of step 1, run 7 in step 6 (905 s). Owner: Sentry (vfork stop, possibly lost wakeup when the child's exec or exit races the parent's stop). A third native run (6) had no engine at all after its second boot (all steps failed within seconds; the second `runsc` started but the API never listened).
- Step 9 fails in two of five native runs (`images before=0 after=N`: the image list is empty before the restart because the images are listed during the run, then the restart returns; the volume data is gone). The container directories are on a tmpfs by design (see round 3), the volume should survive: needs a look.

Conclusion for the default: **do not switch to rewrite**: it is slower by an order of magnitude at start-up and hung in 8 of 8 runs, while native completed 5 of 8 and hung in 2 of the others in `vfork`.

## Flake follow-up (branch fs-flakes)
- **Nil dereference in gofer invalidation (bench).** `disownAllChildrenForInvalidation` appended every entry of `d.children` and read `child.inode`, but outside InteropModeShared negative entries are cached as nil; the watcher invalidates directories in that mode. Fix: skip (and uncount) nil entries. Bench then passed in 29 of 30 legs (10 runs of the three legs of windows-runsc); the one miss was a hang of the rewrite leg after 20 min in a state with two tasks and no Sentry-side stall: the next process never started (the stuck-stub family of docs/results/4-syscall-tests.md), not a crash.
- **/dev/null and config.json flakes: not reproduced.** A stress guest (`test/guests/fsstress.c`, workflow `fs-flakes.yml`, `runsctest -only fsstress`) runs 12 mount namespaces x 600 iterations on a tmpfs and on a host-folder mount: container-directory create+rename+open of `userdata/config.json`, `rootfs/dev` tmpfs with `/dev/null` bind-mounted and opened, an fcntl-locked read-modify-rename data file (the c/storage pattern) with lock-free readers, and an mtime-resolution probe. Clean in native mode over 3 repetitions each. The engine logs of the failed acceptance runs show the failing step was preceded by c/storage losing the container ("Storage for container ... has been removed: unmounting ... container not known", step 3 run 8; "getting container from store ...: container not known" after the failing exec, step 10 run 3); `/dev/null` missing in crun and the missing `config.json` follow from the container's storage being gone, not from a lookup race. Host-folder mtimes repeat for consecutive writes (about 45 to 60 percent of writes share the previous mtime, the tmpfs has distinct ones), which c/storage's change detection tolerates only with its lock counter; not shown to matter.
- **Acceptance x8 on the fix (run 45948349740):** steps 3 and 10 passed in all 7 reported runs (run 3 stopped after step 6). Step 8 failed once on the output check although the compose containers ran (log interleaving); step 5 as known.
- **Rewrite mode crashes at startup for fsstress** (statically linked, gcc 13 glibc): SIGSEGV in libc init right after `set_tid_address`, write at ip 0x431ffb with fault address 0x611, deterministic over 3 runs in `-fsmode rewrite`, no problem in native mode. Owner: platform (winstub rewrite).
