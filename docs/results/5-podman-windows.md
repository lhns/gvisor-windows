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

## Seventh round (branch podman-volume): volume data across an engine restart, and the "no engine" run
Runs (8 repetitions each, default mode, standard user): https://github.com/lhns/gvisor-windows/actions/runs/45944642328 (workaround A) and https://github.com/lhns/gvisor-windows/actions/runs/45946735980 (final).

**Cause of "volume data gone": our own harness workaround, not the filesystem.** Two workarounds from early rounds were still in the engine image and spec after their gaps had been fixed: `static_dir = /run/podman-static` (libpod's database on a tmpfs) and the container directories (`vfs-containers`) on a tmpfs. With the database on a tmpfs, a restart empties it: Podman then knows no volume `accvol` and creates a new, empty one, so `cat /v/f` finds nothing even though the files are on the host folder. Removing the `static_dir` line (the database lives with the storage on the Windows folder, sqlite works there since the delete-rights fix) fixes it; removing the tmpfs on `vfs-containers` (attach sockets on host folders work now) also removed a second flake (`writing runtime spec JSON ... userdata/config.json: no such file or directory`, a just-created directory missing in the tmpfs, seen in 3 of 16 runs with the tmpfs and 0 of 8 without).

**runsc itself flushes correctly.** The test `restart` (`runsctest -only restart`, in the required host-folder step of `windows-runsc.yml`): a first sandbox writes a volume file, a database file and a container directory under a bind mount that has a tmpfs on a subdirectory and calls sync; a second sandbox on the same folder reads them, creates a new container directory and replaces the database file with write and rename: passes. No sandbox shutdown path loses writes (the Windows host-folder backend writes through; there are no dirty inodes in the Sentry to flush), and a restart that kills the old `runsc.exe` first does not leave files held.

**Result of the final 8 runs** (per-step passes of 8; step 5 is the known failure):
| step | passes |
|---|---|
| 1, 2 | 8, 7 (run 2 hung in step 2) |
| 3 run-exit-code | 7 of 7 reported |
| 4 interactive-tty | 7 of 7 |
| 6 bind-mount | 7 of 7 |
| 7 port-publish | 5 of 7 |
| 8 compose | 5 of 7 |
| 9 restart-persist | 5 of 7 (the failures are the two engine deaths below) |
| 10 isolation | 6 of 7 (run 7 hung 1806 s) |
Timings are as before (steps 3 to 10 take 5-90 s each; the acceptance takes 4-5 minutes).

**What still fails is not persistence:**
1. **The AVX-512-looking crash is still there** (runs 3 and 6, and run 5 of the first set, where it killed the engine at both boots): `SIGSEGV addr=0xfffffffffffffff8` in `ScanSpanPackedAVX512` of the guest's Go GC. 3 of 16 runs. After the death, steps 7-9 fail with "cannot connect". Owner: platform.
2. **Hangs, 2 of 8 in the final set** (run 2 in step 2, run 7 in step 10; in run 2 the debug log ends with a gofer range-lock line, in run 7 with `Signal 23` deliveries and no progress); the earlier "native run 6, no engine after the second boot" was the same kind: the first boot stopped between `Using sqlite as database backend` and `API service listening` (normally 3 s), and the second boot was fine (`engine-040939.err` listens). Owner: platform/Sentry (stuck stub, vfork stop).

## Flake follow-up (branch fs-flakes)
- **Nil dereference in gofer invalidation (bench).** `disownAllChildrenForInvalidation` appended every entry of `d.children` and read `child.inode`, but outside InteropModeShared negative entries are cached as nil; the watcher invalidates directories in that mode. Fix: skip (and uncount) nil entries. Bench then passed in 29 of 30 legs (10 runs of the three legs of windows-runsc); the one miss was a hang of the rewrite leg after 20 min in a state with two tasks and no Sentry-side stall: the next process never started (the stuck-stub family of docs/results/4-syscall-tests.md), not a crash.
- **/dev/null and config.json flakes: not reproduced.** A stress guest (`test/guests/fsstress.c`, workflow `fs-flakes.yml`, `runsctest -only fsstress`) runs 12 mount namespaces x 600 iterations on a tmpfs and on a host-folder mount: container-directory create+rename+open of `userdata/config.json`, `rootfs/dev` tmpfs with `/dev/null` bind-mounted and opened, an fcntl-locked read-modify-rename data file (the c/storage pattern) with lock-free readers, and an mtime-resolution probe. Clean in native mode over 3 repetitions each. The engine logs of the failed acceptance runs show the failing step was preceded by c/storage losing the container ("Storage for container ... has been removed: unmounting ... container not known", step 3 run 8; "getting container from store ...: container not known" after the failing exec, step 10 run 3); `/dev/null` missing in crun and the missing `config.json` follow from the container's storage being gone, not from a lookup race. Host-folder mtimes repeat for consecutive writes (about 45 to 60 percent of writes share the previous mtime, the tmpfs has distinct ones), which c/storage's change detection tolerates only with its lock counter; not shown to matter.
- **Acceptance x8 on the fix (run 45948349740):** steps 3 and 10 passed in all 7 reported runs (run 3 stopped after step 6). Step 8 failed once on the output check although the compose containers ran (log interleaving); step 5 as known.
- **Rewrite mode crashes at startup for fsstress** (statically linked, gcc 13 glibc): SIGSEGV in libc init right after `set_tid_address`, write at ip 0x431ffb with fault address 0x611, deterministic over 3 runs in `-fsmode rewrite`, no problem in native mode. Owner: platform (winstub rewrite).

## Flake follow-up 2 (branch fs-mtime)
- **Host-folder mtime.** NTFS gives consecutive writes through short-lived handles the same LastWriteTime (45 to 60 percent of 20000 back-to-back writes repeated the previous mtime; tmpfs: none). Outside InteropModeShared the Sentry took mtime and ctime from the host on every stat and never touched them on a write. Now `winInode.noteWrite` records the Sentry time of each guest write and `updateFromAttrLocked` reports `max(host, last write)` for mtime and ctime. `fsstress` fails when consecutive writes repeat or go backwards (workflow fs-flakes: 0 repeats in 20000 writes on tmpfs and host folder).
- **c/storage lock + rewrite + reload.** `fsstress` section D is that pattern (fcntl F_SETLKW write and read locks on a lock file whose first 8 bytes are the write counter, data file rewritten by temporary file + fsync + rename, lock-free readers, 12 processes x 600 rounds): no lost update, no stale counter, no torn read on tmpfs or host folder, native and rewrite mode.
- **"container not known" is not the cause of the failures.** In run 45948349740 the warning `Storage for container ... has been removed: ... container not known` appears in 2 of 7 runs that passed all steps (it is logged when `rm -f acc-a acc-b` removes two containers at once). In the failed step 3 (run 45934083189 run 8) the container finished and was removed by `--rm` before the attach request was handled (the engine log shows create at :46, the attach error at :50 after the network setup), a race in podman's remote `run` that the sandbox's slowness widens.
- **Step 10 failure reproduced (run 45954620402, run 8):** `exec acc-b sh -c 'ls /proc | grep -c ...'` failed with `container creation timeout: internal libpod error`; the Sentry log shows nothing happening for 123 s (07:08:31 to 07:10:34) except Go preemption signals in the engine (pid 1) and in a second Go process (pid 123, started at 07:08:31, 10 threads) that created no child. Both Go processes were alive and idle, which looks like a deadlock between the podman service and a helper process (a cross-process lock, libpod's shared-memory lock or a c/storage file lock) or a stuck process start; the log has no goroutine dump to tell which. Next step for whoever continues: on an exec timeout send SIGQUIT to the helper (and the service) and keep their stderr.

## Fifth round (branch podman-build): the copied library, apt on a host folder
CI: acceptance https://github.com/lhns/gvisor-windows/actions/runs/45944580834 and 45946602769 (steps 1-3, 6-10 pass), reproducers `windows-runsc` run 45942854421 (`cfr`, `debian`), diagnostics `podman-diag` runs 45934250418, 45936045107, 45947921034, 45949443675, 45954271047.

| step | result | what happens |
|---|---|---|
| 1, 2, 3, 6, 7, 8, 9, 10 | PASS | (9 and 10 failed once each in the runs where the engine's own store was damaged, see below) |
| 4 interactive-tty | FAIL | unchanged: the Windows client says `The input device is not a TTY` in the test's ConPTY |
| 5 build-apt | FAIL | `libc.so.6: file too short` is gone; the debian RUN now fails inside `apt-get` on the host folder (below) |

**`libc.so.6: file too short` (fs-copied-file-too-short): not a copy problem.** The experiments (`test/guests/cfr.c`, `runsctest -only cfr`, required step) show a file written with `copy_file_range` (and chown, chmod, utimens) on a host folder reads back byte for byte from the same process, another process, `mmap`, `pread` and `exec`, with the folder revalidated or trusted, also after the cache was cut into pieces by earlier reads. The copy is intact in the Podman storage too (same sha256 and size as the image layer). What fails is Debian's `ld-linux-x86-64.so.2` run from a host folder: its `open` of the program is followed by no `read` at all in the strace, then the error. The same loader runs from tmpfs (`windows-debian-guest.yml`, `sentryguest`, prints `hi`; and in `runsc` from `/tmp`). So the `syscall` instruction of the loader's `read` was not patched: the host-folder cache is filled in 64 KB windows, `mm` therefore maps one text VMA in several ranges (one per cache segment and per fault), the `winstub` patcher works on one `MapFile` range at a time and does not patch a window that straddles two ranges (known limit since 1d), and the raw `syscall` then runs on Windows and returns without reaching the Sentry. Fix in the Sentry (`fsimpl/gofer`, `Translate`): an access that executes fills and translates the whole VMA, not 64 KB, so the text is one cache segment and one pma. Test `debian` (Debian's loader and `echo` from a bind mount and from the root folder): the bind mount now passes, and the Podman acceptance gets past the libc error. **Still failing: the same loader from the root folder (a lower layer of the root overlay)**: a first `read` of the cache for the ELF header makes an extra segment, and the VMA is still mapped as two pmas ([0x1000,0x10000) and [0x10000,0x27000) in the log). For the platform owner: the patcher must patch a `syscall` window that spans two adjacent `MapFile` ranges (keep the last bytes of the previous range of the same file mapping and rescan), or the Sentry cannot guarantee single-range text. Reproducer: `windows-runsc` test `debian`, failing on `/dbin`.

**AVX-512 crash of the guest Go runtime (round 4, item 4): the CPUID mask did nothing.** The guest executes `cpuid` itself (there is no CPUID faulting on Windows), so the Sentry's feature set does not reach it; the mask was removed again. What works is the Go runtime's own switch: `GODEBUG=cpu.avx512f=off,cpu.avx512cd=off,cpu.avx512bw=off,cpu.avx512dq=off,cpu.avx512vl=off` in the engine environment (`start-engine.ps1`; the name `cpu.avx512` alone is rejected). No engine death in the runs since. The platform defect underneath (a sporadic fault in AVX-512 code, probably lost vector state after a stop) is still open for the platform owner.

**apt on a host folder (step 5, open).** Three symptoms of one area, intermittent between runs:
- `rename failed, Invalid argument` of `/var/lib/apt/lists/partial/..._bookworm-updates_InRelease` into `lists/` (apt-get `strace`, and acceptance): winhost's `renameHandle` returns EINVAL; the raw error is `The filename, directory name, or volume label syntax is incorrect` (ERROR_INVALID_NAME) for the NT path `\??\C:\...fs\dir\<layer>ar\libpt\lists\deb.debian.org_debian_dists_bookworm-updates_InRelease` (182 characters); the first rename of the same run, to the neighbouring `..._bookworm_InRelease`, succeeded. `renameHandle` now retries a refused rename with the `\?\` form and logs the status (`winhost: rename to ...`); that retry was not exercised yet (the later runs hit the second symptom).
- `dpkg: error: cannot access archive '/var/cache/apt/archives/libmagic-mgc_1%3a5.44-3_amd64.deb'` (or `cannot stat .../apt-dpkg-install-X/00-libmagic-mgc...`) although apt's `rename` of the file into place returned 0.
- the engine's own store: `error removing stale temp dir: unlinkat .../var/cache/apt/archives: directory not empty` (RemoveAll of a layer directory left by a failed build), which then makes later requests fail (steps 9 and 10 in one run).
Not reproduced by scripted equivalents (`runsctest -only hostfs`, required: symlinks absolute, relative and with `%3a` names, hard links, rename after chown/touch -d/chmod, empty file, a name looked up before it is renamed into place, FIFO). What these have in common is a name in a directory of a container layer that is renamed, deleted or listed while the engine also works on the same storage (two processes, several mounts of the same folder). Next step: run the `podman-diag` apt install with `--strace` limited to `openat`, `newfstatat`, `getdents64`, `rename` and `unlinkat` and compare the engine's and the container's view of one directory.

**Step 10 flake (`acc-b sees` the other container's file).** Not reproduced: passed in every run of this round. The one failing run (45946602769 and earlier 45942854382) failed because the engine's store was already damaged, not on the isolation check.

Verdict: the truncated libc was never a copy problem but unpatched `syscall` instructions in text mapped in pieces, mitigated in the gofer (bind mounts) and open for the root-folder layers in the platform patcher; the build step now stops in `apt` on a name-dependent rename or lookup failure on the host folder that the scripted tests do not reproduce.

## Eighth round (branch podman-measure): eight runs on `windows` with the decoder fix and bounded stub waits
Runs: https://github.com/lhns/gvisor-windows/actions/runs/45952884948 (default: native, AVX-512 hidden), https://github.com/lhns/gvisor-windows/actions/runs/45956915374 (same plus `GODEBUG=cpu.avx512=off` in the engine's environment). Each engine start now writes a goroutine and winstub state dump after 6 minutes (`RUNSC_HANG_DUMP`, `hang-<time>.txt` next to the engine logs).

**Default, 8 runs.** Complete (all 10 steps run, step 5 the known failure): runs 1, 3, 4, 5, 7. Per-step passes of 8 (a step that never ran counts as not passed):
| step | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 |
|---|---|---|---|---|---|---|---|---|---|---|
| passes | 7 | 7 | 7 | 7 | 0 | 7 | 6 | 6 | 5 | 5 |
Crashes: **1 of 8 runs still has the guest GC crash** (run 2, second engine boot: `SIGSEGV addr=0xfffffffffffffff8 ... ScanSpanPackedAVX512 scan_amd64.go:31 +0x3b`, the same PC offset as every earlier crash), so the decoder fix did not remove it. Hangs: **3 of 8** (run 2 step 1 took 901 s, run 6 step 2 hung 900 s and the job then stalled, run 8 step 7 hung 1800 s). Timings of healthy steps are unchanged (step 7 45-97 s, steps 3, 4, 6, 8, 9, 10 6-30 s).

**What the hang dumps show** (runs 2, 6, 8; 60-95 KB each): the Sentry is idle: 7 to 12 task goroutines in `futexWaitDuration` (the guest's threads asleep on futexes), the kernel's main goroutine waiting, no goroutine inside a stub wait. The winstub state of run 8 shows all stub threads of the container's address spaces parked in `NtWaitForSingleObject` (state 1, stubSleep 1, nothing published), i.e. no stuck stub in the sense of the bounded waits; one address space (pid 3020) has `last debug event ... code 0xc0000005 ... ContinueDebugEvent ok=false err=The parameter is incorrect`, `replaced threads 3`, a thread in state 0 with ctlSleep 1 and one in state 2 (request published, not consumed) at rip 0x1100c3 with 300 ms cycles. So the hangs look like a lost wakeup after a replaced or killed thread (the `ContinueDebugEvent` failure) rather than a stub that does not run. Owner: platform.

**The GC crash.** With `GODEBUG=cpu.avx512=off` in the engine process the crash still happens (2 of 8 runs, both at engine start, same `ScanSpanPackedAVX512` +0x3b), so Go's `internal/runtime/gc/scan` selects its AVX-512 path regardless of that setting (or the variable did not reach the process: not verified). It faults at the first vector instruction of the function with `sigcode=1` and the fixed address `-8`: either an AVX-512 instruction with no enabled state in the thread (an illegal instruction reported as a memory fault) or a wrong register state across a stop. The failure is thread-dependent (m=3, 4, 5, 16, 17). A direct test: a static Go (1.25+, greenteagc) guest that allocates in a loop on a CPU with AVX-512, run through `runsc run` with `WINSTUB_KEEP_AVX512` unset, no podman involved.

## Ninth round (windows 74e8f4a70: integrated platform stability, podman build and measure)

Run https://github.com/lhns/gvisor-windows/actions/runs/45962927170, 8 runs per mode, Podman 6.2.0, standard user.

| mode | runs passing every step but 5 | step 5 | other failures |
|---|---|---|---|
| native | 7 of 8 | 0 of 8 | run 3: engine did not answer after the restart (step 9), then step 10 |
| rewrite | 5 of 8 | 0 of 8 | run 1: engine SIGSEGV addr=0x28 in cgo at start; runs 3 and 6: no answer after the restart (step 8 once) |

- No hangs and no Go GC crash (`ScanSpanPackedAVX512`) in 16 runs.
- Step 5 now fails inside dpkg: `cannot stat pathname '/tmp/apt-dpkg-install-.../*.deb': No such file or directory` after apt moved the archive.
- The restart failures share one cause: after the second boot c/storage reports `loading primary layer store ... may be corrupted`, so the layer store written before the restart is unreadable (images before=3, after=0).
- Rewrite run 1: `SIGSEGV addr=0x28` during cgo at engine start, which looks like a `%fs:0x28` (stack canary) access with a zero base in a library — an unrewritten `%fs` site.

Verdict: with the hang fixes and the integrated build fixes, native mode passes every acceptance step except the build in 7 of 8 runs with no hangs or GC crashes, and the remaining failures are a corrupted layer store after restart, dpkg losing apt's archives on the storage folder, and one unrewritten %fs site in rewrite mode.

## Tenth round (branch podman-bar: graceful stop, workarounds audit, 8 runs per mode)

Runs: https://github.com/lhns/gvisor-windows/actions/runs/45969933641 (before merging `windows` 17a48255f; GODEBUG and `BUILDAH_ISOLATION` removed, so the build failed in pasta again) and https://github.com/lhns/gvisor-windows/actions/runs/45973373703 (podman-bar 09831c180 = merged `windows`; `BUILDAH_ISOLATION=oci` kept, no GODEBUG). Podman 6.2.0, standard user, 8 runs per mode.

**Graceful stop.** `Stop-Engine` now runs `runsc kill podman-<name> TERM` and waits 60 s before it kills. `runsc run` now serves the control pipe (ADR 0019) for `state` and `kill`, so the signal reaches init (podman). The engine logs show `Invoking shutdown handler` at each restart. The Linux harness is not graceful either: `docker rm -f` kills, the native one uses `pkill` (SIGTERM).

**The "primary layer store" failure (step 9 after restart) is not the hard kill.** Run 45962927170 native-3: the log `engine-081151.err` is the second boot. It starts in 2 s and answers `_ping` at once, but every `version` returns 500 from 08:11:55 until the harness gives up at 08:16:55 (the 08:16:58 answers are step 10, not a startup stall): `loading primary layer store data: error removing stale temp dir: unlinkat .../vfs/tempdirs/temp-dir-.../var/cache/apt/archives: directory not empty`. The same error is already in the first boot, in step 5: the failed build's `deleting build container ... removing temp dir failed: unlinkat .../apt/archives: directory not empty`. So a failed apt build leaves a layer directory that `RemoveAll` cannot delete (the directory lists as empty, the host says not empty: likely a file in delete-pending state, an open handle in the gofer, or a name readdir does not return), and c/storage retries that removal on every load and fails the whole store. Confirmed by 45973373703: graceful stop was in effect, and step 9 failed in exactly the 3 native runs whose step 5 ended with `deleting build container` (runs 2, 3, 6); the others passed step 9; in the first run of this round (build failing early in pasta, no layer dir) step 9 passed in all 16 runs. Owner: `fsimpl/gofer` / winhost (rmdir after RemoveAll on a host folder), reproducer: `apt-get install` in a debian container on the storage folder, then `rm -r` of the layer.

**Workarounds audit** (engine setup vs `engine-runsc.sh`):
| item | Windows | Linux harness | decision |
|---|---|---|---|
| GODEBUG avx512 off, `CONTAINER_NOGODEBUG=1` | set | not set | removed (the second is read by nothing) |
| `CONTAINERS_CONF` | set | file + drop-in, no env | removed (same file at `/etc/containers/containers.conf`) |
| capabilities | 38 caps, no BPF, PERFMON, CHECKPOINT_RESTORE | `--cap-add all` | all 41 now; engine sees `CapEff 000001ffffffdfff` (NET_RAW stripped by runsc, as on Linux) |
| `BUILDAH_ISOLATION=oci` | set | not set | kept, see below |
| storage driver vfs | vfs | overlay | kept: overlay on a host-folder mount is not supported by the Sentry (open) |
| `--overlay2=root:memory`, `--in-sandbox-cgroup=v2`, sysfs/cgroup2/devpts mounts, `/var/tmp` tmpfs, resolv.conf, devices | set | docker provides them | needed by the missing runtime layer (no docker); not removable |
| `--log-level=info`, `--debug`, hang dump | set | not set | diagnostics only |

**Why buildah thinks it is rootless: not found.** `engine-env.txt` (new, from the engine's own process) shows euid 0, `uid_map = 0 0 4294967295`, `CapEff` with CAP_SYS_ADMIN, no `_CONTAINERS_*` variables, `NoNewPrivs 0`, `cap_last_cap 40`. Those are all the inputs of `unshare.IsRootless()` (c/storage: `_CONTAINERS_ROOTLESS_UID`/euid, `_CONTAINERS_USERNS_CONFIGURED`, CAP_SYS_ADMIN through capget, "4294967295" in uid_map) and all say rootful; capget in the Sentry is correct and the engine logs no "Failed to read CAP_SYS_ADMIN" warning. The client sends `isolation=0` (default) and `networkmode=0` (access log), so buildah's default resolves server side to OCIRootless, as pasta is only used for that isolation. Without `BUILDAH_ISOLATION` all 16 builds of 45969933641 failed with `pasta ... Couldn't switch to pasta namespaces`. Next step: run the engine with `--log-level=debug` and log buildah's isolation choice, or a Go probe calling `unshare.IsRootless()` in the sandbox.

**Eight runs per mode on 09831c180 (45973373703).** Steps passing of 8:
| step | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 |
|---|---|---|---|---|---|---|---|---|---|---|
| native | 8 | 8 | 8 | 8 | 0 | 8 | 8 | 8 | 5 | 5 |
| rewrite | 8 | 6 | 6 | 6 | 0 | 6 | 6 | 6 | 6 | 8 |
Step times: native 2 s, pull 17 to 37 s, run 5 to 10 s, tty 20 s, bind 9 to 18 s, port 41 to 92 s, compose 14 to 44 s, restart 14 to 18 s (failing: 307 s timeout). Step 5 now reaches apt: native runs end in `dpkg: cannot stat '/tmp/apt-dpkg-install-.../*.deb'` (or the temp-dir error above); four rewrite runs die in `apt` with `Segmentation fault` (exit 139) or `double free or corruption` (134).
- **No Go GC crash** (`ScanSpanPackedAVX512`) in 16 runs with the GODEBUG line gone, and no hang or hang dump. (The previous run, before the merge, also had none.)
- **Rewrite runs 4 and 6: engine SIGSEGV addr=0x28 in cgo (`sqlite3_exec_no_args`) at start**, the unrewritten `%fs:0x28` site of round 9 (the same in 45969933641 run 7); steps 2 to 9 then fail with "cannot connect". 3 of 24 rewrite runs in the two sets, 0 of 16 native.

Verdict: the engine now stops gracefully and survives without the GODEBUG line (no GC crash in 16 runs), but the restart failures come from a layer directory a failed apt build leaves behind and that the host folder will not delete (gofer/winhost, step 5's own failure family), and why buildah chooses rootless isolation is still unexplained (all its inputs look rootful), so `BUILDAH_ISOLATION=oci` stays.

## Rename atomicity and mounts on host folders (branch fs-rename)
- **Every c/storage rewrite of layers.json/images.json was delete + rename.** Logging the paths (`winhost: rename ... replacing refused (permission denied), deleting the target first`) showed the replacing rename refused with access denied for every variant (POSIX, plain, classic) whenever the guest had had the target open. Cause, found with `TestRenameReplaceProbe` (windows-fs, as a standard user): a handle opened by file ID on the target, with any access and full sharing, makes NTFS refuse to replace it; handles opened by name do not. The backend's pin of a file a guest holds open is such an ID handle. The delete + rename window leaves the file missing: readers see an empty store (the "container not known" / "may be corrupted" symptoms), a stop in it loses the layers. Fix: a pin is switched to a by-name handle (`FS.pinByName`) just before its file is replaced; by-name handles stay out of the way otherwise because they block renaming the directories above (a first version that pinned by name always broke deleting layer directories: run 45971886285, steps 5, 8 and 9 failed). Tests: `TestRenameReplaceProbe`, `TestRenameDirectoryOverPinnedFiles`, and the guest `aptrename` (layers.json replaced while another process reads it, apt's lists/partial pattern with long names, dpkg's install directory with absolute symlinks to `%3a` archives, trees removed while a name is held open): no fallback is logged any more.
- **Mounts vanished when the folder's change stream overflowed.** `dropAllCaches` (overflow) and event-driven `invalidate` call `vfs.InvalidateDentry`, which unmounts what is mounted on the dentry, and a dentry dropped from its parent is replaced by one without the mount: a container's `/dev` or `/tmp` (tmpfs on a directory of the storage folder) disappeared (`crun: open /dev/null: No such file or directory`, a file of another container visible). The acceptance logs show 3 to 4 overflows per run. Fix: `invalidate` keeps a dentry that has a mount on it or below it and invalidates around it (`invalidate_mounts.go`, `vfs.Dentry.IsMounted`). The `mountkeep` test (flood from Windows while a tmpfs is mounted) did not fail without the fix (the overflow it needs is not reliable), so the evidence is the logs: acceptance run 45966300052, 8 runs with 3 to 4 overflows and up to 20 invalidations around mounts each, all 10 steps passed in all 8.
- **Acceptance x8 on the final state (run 45973673020): all 10 steps passed in all 8 runs; no rename fallback in any engine log.**
- Not reproduced by scripts: the `cannot stat ... apt-dpkg-install-*` error of dpkg (absolute symlinks to archives with `%3a` created and resolved from the guest work), and an `EINVAL` of a long NT path (renames now go through NtSetInformationFile and retry relative to the directory; neither was needed in these runs).
- `aptrename` crashes with `free(): invalid size` in rewrite mode only (native passes): a platform defect, as with `fsstress` before.

## Eleventh round: the bar in native mode (2026-11-27)

On `windows` with atomic replace, mount-safe invalidation, graceful stop and the init user-namespace inode, native mode passed all ten steps in 8 of 8 runs twice (run 45976796134, and 45985526511 without `BUILDAH_ISOLATION`), and step 5 alone in 80 of 80 runs. Buildah's rootless choice came from `/proc/self/ns/user` not having Linux's fixed inode 0xEFFFFFFD. Rewrite mode still fails step 5 (glibc heap errors) and sometimes engine start (an unrewritten `%fs` site in sqlite).

Verdict: an unmodified Podman 6.2.0 engine passes all ten acceptance steps on gVisor on Windows as a standard user in native mode, 8 of 8 runs twice, under the conditions listed in ADR 0002; CPUs without FSGSBASE (rewrite mode) do not pass yet.

## Twelfth round: all CPU modes on the merged head (2026-11-27)

Run 46041026851 on `windows` 233a84a4e, all ten steps required in every mode: native 8 of 8, rewrite 8 of 8, rewrite-fxsave 8 of 8. On the development VM (no FSGSBASE, XSAVE or AVX), `aptrename` passed 200 rounds 5 of 5 with that build's `runsc.exe`.

Verdict: an unmodified Podman 6.2.0 engine passes all ten acceptance steps on gVisor on Windows as a standard user in all three CPU modes, 24 of 24 runs on the merged head, under the conditions listed in ADR 0002.

## Thirteenth round: overlay default, speed work and upstream merge together (2026-11-27)

Run 46064357443 on `windows` 030754bf2 (overlay as the default storage driver, ADR 0021; fork+exec work, ADR 0022; the first upstream merge), all ten steps required: native 8 of 8, rewrite 8 of 8, rewrite-fxsave 8 of 8. windows-stability on the same head passed.

Verdict: with overlay as the default storage driver and the fork+exec changes, an unmodified Podman 6.2.0 engine still passes all ten acceptance steps in all three CPU modes, 24 of 24 runs.
