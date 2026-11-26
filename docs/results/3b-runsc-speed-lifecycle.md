# 3b. runsc.exe: end-to-end speed and the container lifecycle commands

Nodes: "Measure end-to-end speed on Windows" (workloads-end-to-end-speed), "Container lifecycle commands on Windows" (runsc-windows-lifecycle-commands). Decision: ADR 0019. Everything ran on `windows-latest` as the standard user `ci-std` (workflow `windows-runsc.yml`, branch `runsc-speed-lifecycle`): https://github.com/lhns/gvisor-windows/actions/runs/45912562847

## Speed
The Phase 0 workloads (`test/e2e/podman-acceptance/harness/bench-inner.sh`) as a busybox script, `tools/windows/bench/bench.sh`, timed inside the guest with `/proc/uptime`: untar of the Phase 0 tarball (20k small files, 2000 directories) onto the root filesystem (memory overlay on a host folder), onto tmpfs and onto a host-folder bind mount; `rm -rf` of the bind-mounted copy; sha256 of 1 GiB and gzip -6 of 512 MiB from `/dev/zero`; a fork+exec loop of `/bin/true`. The Windows run is one run on a shared runner; run-to-run noise is large (Phase 0 saw up to 1.3x).

"native (Linux, busybox)" is the same script and busybox on the Linux runner of the same workflow. "Linux runsc / Linux native" are the numbers of `docs/results/0a-linux.md` (GNU tools in a debian container, a different tool set, so only ratios are comparable).

| workload | native (Linux, busybox) | runsc.exe on Windows | ratio | Linux runsc / Linux native, 0a |
|---|---|---|---|---|
| untar 20k files, container rootfs | 1.29 s | 22.65 s | 17.6x | 0.75 / 0.67 = 1.1x |
| untar 20k files, tmpfs | 1.29 s | 20.87 s | 16.2x | 0.51 / 0.69 = 0.7x |
| untar 20k files, host-folder bind mount | 1.36 s | 123.35 s | 91x | 2.73 / 0.77 = 3.5x |
| rm -rf 20k files, bind mount | 0.20 s | 34.68 s | 173x | 1.24 / 0.40 = 3.1x |
| sha256 of 1 GiB | 3.56 s | 8.50 s | 2.4x | 6.02 / 0.91 = 6.6x |
| gzip -6 of 512 MiB | 5.02 s | 5.64 s | 1.1x | 3.81 / 1.37 = 2.8x |
| fork+exec of /bin/true, per exec | 0.46 ms | 32 ms (100 execs: 3.23 s) | 70x | 2.6 ms (2000: 5.27 s) / 0.5 ms = 5x |
| `runsc run` of a hello, start to exit | | 0.25 s median (0.12 s fastest of 5) | | |

Findings:
- CPU-bound work is at native speed: gzip is within 10 percent, sha256 2.4x (the first part of it is the pipe from `head`; the Linux runsc ratio was the same shape).
- Everything that is many small syscalls is slow, and it is slow in the Sentry itself, not in the host folder: untar onto tmpfs (no host file involved) takes 20.9 s, 16x native and 30x Linux runsc, so a file create costs about 1 ms. The host-folder bind mount adds another factor of 6 (123 s, 6 ms per file; reads and writes open and close the file for every call, ADR 0005) and `rm` costs 1.7 ms per file.
- Process creation costs 32 ms per fork+exec (a new stub process per address space). The Phase 0 loop of 2000 would take about 65 s, so the loop ran with 100 iterations (an earlier try with 2000 hit the 20 minute limit of the step with nothing printed, because the loop was the first workload). Start-up of a whole `runsc run` is 0.25 s.
- Where the time goes was not profiled (the platform worker's, the Sentry's and the fs worker's areas).

## Lifecycle commands (ADR 0019)
`create` starts a detached `runsc.exe sandbox` process, which boots the Loader, serves `\.\pipe\runsc-<id>` (current user only) and writes `%LOCALAPPDATA%\runsc\<id>\state.json` (or `--root`). `start`, `kill`, `ps`, `wait`, `exec` go through the pipe, `state`, `list`, `delete` through the state file. Test `lifecycle` (`tools/windows/runsctest`, a required step of `windows-runsc`, as the standard user) on a container running `echo up; sleep 300`, passes in 7.6 s:

create; `state` is `created`; `exec` before `start` fails; `start`; `state` is `running`; `exec sh -c 'echo hi from exec; exit 5'` prints and returns 5; `ps` lists `sleep`; `list` shows the container; `delete` of a running container fails; `kill KILL`; `wait` returns 137; `state` is `stopped` with the exit code; `delete`; `state` fails. The first run found that `delete` raced the sandbox's 2 s exit linger (the sandbox holds the log files); `delete` now waits for it.

Not done: containers with a terminal through `create` (use `run`), `pause`, `resume`, `events`, `spec`, `do`, `checkpoint`, `restore`; the container's stdout and stderr go to log files in the state folder (a runtime that wants them as handles, like conmon, needs more); the detached sandbox may be killed with its creator's job object where the runner forbids breakaway (not seen on windows-latest).

Verdict: runsc.exe now runs the full create/start/exec/kill/delete lifecycle on a detached sandbox with a user-only control pipe, and CPU-bound work runs at native speed, but small-file and process-creation workloads are 16x to 170x slower than native (about 100x slower than Linux runsc for a bind mount), so the next work is Sentry and host-folder syscall cost, not runsc.
