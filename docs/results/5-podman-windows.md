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
