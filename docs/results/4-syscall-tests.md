# 4. gVisor's syscall tests on the Windows port

Node: "gVisor syscall tests on Windows" (ADR 0009). The suite of `test/syscalls/linux` (C++ gtest) is built from google/gvisor at c3efd8c93, the commit the go branch was generated from, with Bazel in Linux CI, statically linked, and run as guests: under runsc (systrap) on Linux as the baseline, and under the Sentry on Windows (`tools/windows/sentryguest`, the winstub platform). Everything ran in CI; workflow `.github/workflows/syscall-tests.yml` (dispatch, weekly schedule, quick subset on a push to the branch).

Main run (Sentry with a network stack): https://github.com/lhns/gvisor-windows/actions/runs/45909336357 (report artifact `syscalltests-report`). The same suite without a network stack: https://github.com/lhns/gvisor-windows/actions/runs/45917614175. The per-binary table is `docs/results/4-syscall-tests-per-binary.csv` (256 rows); the per-case results (14620 rows per side) and the output tails of every failure are in the artifacts `results-linux-*`, `results-windows-*` and `syscalltests-report`.

## How it is built and run
- **Build** (13 minutes with a warm cache, 257 of 260 test binaries; `mount_test`, `sticky_test` and the helper `exec_proc_exe_workload` do not build): Bazel 9.3.1, `--config=x86_64 -c opt --strip=always`, the static link options of `tools/bazeldefs/cc.bzl` (`-static -lstdc++ --whole-archive -lpthread`). On the way: the crosstool links with `-fuse-ld=gold`, which current Ubuntu no longer has (an `ld.gold` symlink to bfd), and the link actions need a `PATH`. 14620 test cases (`--gtest_list_tests` of every binary, run natively).
- **Guest driver** (`tools/windows/syscalltests/driver.c`, static C): reads `binary<TAB>test` lines and runs each case as `/bin/binary --gtest_filter=<case>` in its own `TEST_TMPDIR` with a 60 s timeout, printing `S` (start), `R` (result: PASS FAIL SKIP TIMEOUT SIGNAL) and the output tail of failures. The environment is the one of upstream's runner (`TEST_TMPDIR`, `TEST_ON_GVISOR=systrap`, `TEST_SRCDIR=/runfiles`). No sharding by gtest, no upstream blocklists: a case that fails on both sides is part of the baseline.
- **Shards**: 12, balanced by case count, each with its own root file system tar (the shard's binaries, driver, busybox, `/etc`). Linux: `docker import` of the tar, tree-built runsc as a docker runtime with `--platform=systrap`, `--network none`. Windows: `sentryguest -tar … -sysmounts -net -stream` (new flags: /proc, /sys, /dev and /dev/pts mounted as runsc does; lo and eth0 behind the windows-nat NAT with netfilter and netlink sockets; guest output streamed every 20 ms so a crash loses nothing). One kernel runs all cases of a shard; when the Sentry dies or hangs, the case named by the last `S` line is marked CRASH or HANG (no output for 240 s), and the rest runs in a fresh kernel; after 4 crashes or hangs of one binary the rest of that binary is marked the same without running (same cause assumed).
- A "Windows-only failure" is a case that PASSed or was SKIPped on Linux and did not PASS or SKIP on Windows.

## Result
| | Linux runsc | Windows Sentry (with network) | Windows Sentry (no network) |
|---|---|---|---|
| test cases | 14620 | 14620 | 14620 |
| PASS | 11918 (81.5%) | 11298 (77.3%) | 8983 (61.4%) |
| SKIP | 1571 | 329 | 294 |
| FAIL | 1117 | 1097 | 4120 |
| TIMEOUT / SIGNAL | 3 / 11 | 77 / 86 | 16 / 74 |
| CRASH / HANG (Sentry died or stalled) | 0 / 0 | 917 / 816 | 609 / 524 |

- Windows-only failures: **2549** of 14620 cases (with network); the Windows port passes **83.6%** of the cases that pass on Linux (72.2% without network). 138 of 256 binaries pass completely on Windows; 80 binaries have Windows-only failures.
- The 14620 cases include the ones that fail on both sides (upstream expects them to fail in this harness: no real cgroups, devices and so on), so the pass rates are lower than upstream's CI; the gap, not the absolute rate, is the result.

## Windows-only failures by cause (with network)
| cases | cause |
|---|---|
| 1,685 (two binaries) | Not the netpoller and not a handle leak: after a few hundred to a thousand guest processes one stub process never starts and the Sentry waits for it forever (`socket_filesystem_test`, `socket_abstract_test` and others; see "The Sentry stall" below). Owner: platform (winstub). |
| 180 | cgroup, cgroup2 and cgroup2-ebpf tests: no cgroup file systems in the harness (Linux skips them). |
| 175 | Features the harness does not provide: overlay mounts (`overlay_test` 75), POSIX ACLs on tmpfs (74), `/dev/fuse` (24), `/dev/net/tun` (23), bridge, network namespaces, mqueue. |
| 107 | ptrace and process control: `ptrace_test` 32, `wait_test` 37, `exec_binary_test` 28, `exec_test` 10. winstub does not support ptrace (`AddressSpace.PrepareForPtrace` returns an error) and these cases need it or its stops. |
| 80 | UDP multicast and unbound-socket cases (`socket_ipv4_udp_unbound_loopback_test` 76, datagram-based unbound 4): probably the NAT's eth0 differs from the loopback-only sandbox of the baseline, so group membership and default multicast interface answers differ (not investigated). |
| 74 | Timeouts: `socket_stream_blocking_tcp_test` 48 (blocking TCP read/write over the NAT), `socket_stress_test` 13, `udp_bind_test` 7. |
| 63 | netlink route and device views: `socket_netlink_route_test` 49 (dumps of links and addresses), `socket_netdevice_test` 6, `packet_mmap_test` 8. |
| 41 | The guest dies with SIGSEGV (signal 11) when a signal arrives while it is blocked or on SIGPIPE: `pipe_test` 18 (writes to a closed pipe), `sigtimedwait_test` 11, `poll_test` 4, `proc_test` 4, `signalfd_test` 4. Owner: platform (signal delivery to a blocked task). |
| 12 | `arch_prctl_test` (`GetSetGS`, `ConcurrentGetSetGS`, `ConcurrentPatchAndUnpatchRace`, ...): the Sentry panics with `Unknown syscall 158 error: winstub: unsupported`. The call is `arch_prctl(ARCH_SET_GS = 0x1001, ...)`: the handler calls `t.UserModifiedGS()`, which reaches `winstub.addressSpace.UserModifiedGS` and returns the raw `errUnsupported` (the trampoline owns %gs for the TEB). A plain error that is no errno makes the syscall layer panic instead of failing the call; returning EPERM/ENOTSUP would turn these 12 crashes into 12 clean failures, and `PrepareForPtrace` has the same defect. Owner: winstub. |
| ~120 | Other single-check failures (`proc_net_test`, `sysret_test`, `exceptions_test`, `mq_test`, `stat_test`, …), each under 10 cases. |

## The Sentry stall (sentry-netpoll-failure)

The defect was filed as "Sentry dies with netpoll failed". Findings from instrumented runs on windows-latest (workflow `sentry-netpoll.yml`, `sentryguest -handles 5s -stall-dump 90s`):

- Handle count is flat. Over 1500 cases of `socket_filesystem_test` in one kernel the Sentry process holds 180 to 240 handles from the first minute to the last, 20 to 25 goroutines, a constant 56 to 61 MB commit. There is no per-guest-process leak of handles, sections, events or pipes. (`GetProcessHandleCount` is logged every 5 s with `-handles`.)
- One run of `socket_filesystem_test` passed completely: 1500 cases, 1474 PASS, 26 SKIP, DONE 1500, exit 0. The other runs stalled (`socket_filesystem_test` after 75, 239 and 148 cases, `socket_abstract_test` after 1014): no output, no crash. `fatal error: runtime: netpoll failed` did not reproduce in any of the five instrumented runs, so its cause stays open (see hypothesis below).
- The stall is in winstub. The goroutine dump (`-stall-dump`) shows one guest task in `execve` -> `mm.NewMemoryManager` -> `winstub.(*Platform).newAS` -> `addressSpace.Release` blocked on `<-as.loopDone`, and that address space's `debugLoop` goroutine blocked in `WaitForDebugEvent(INFINITE)` for minutes. So `newAS` waited its 30 s for the anchor thread, gave up, called `Release` (`TerminateProcess`), and the debug loop never saw the process exit.
- The process list (`NtQuerySystemInformation`, thread state/wait reason) shows the stub of that address space alive after 90 s to 20 min with a single thread in an executive wait (state 5, reason 0), unlike healthy stubs (two threads, reason 6 = user request). `-stall-kill` terminates it from outside: `TerminateProcess` returns and the process stays, so the thread is stuck inside the kernel (a debug-event handshake or a file-system filter such as Defender holding the freshly written `winstub-<n>.exe`; not told apart here).
- Counts: the process list was taken in the last two stalled runs (148 and 1014 cases): exactly one stuck stub each, all other stubs healthy. Stalls came after 75, 239, 148 and 1014 cases; one run got through 1500, so roughly one stuck stub per 100 to 1500 guest processes.

What to change in winstub (not done here, owned by the platform worker): do not wait unboundedly in `newAS`/`Release`. On the 30 s timeout abandon the stuck address space (leak the process and the debug loop goroutine, do not wait on `loopDone`) and create a new one, so one stuck process costs 30 s and one process instead of the whole kernel. A retry at the `execve` level is the same idea.

Hypothesis for the original netpoll crash (not verified): `GetQueuedCompletionStatusEx` fails with ERROR_INVALID_HANDLE if Go's IOCP handle value is closed and reused. winstub closes raw handle values in several places (`Release` closing `evReq`/`evReply`/`hThread` of every slot, the LOAD_DLL/CREATE_PROCESS file handles in `debugLoop`, `freeThread`); a second close of a value that was meanwhile reissued would close an unrelated handle, and in a long run the IOCP handle is one of the candidates. Worth auditing for double closes. The harness now keeps the Go error line (`runtime: GetQueuedCompletionStatusEx failed (errno=...)`) in run_shard.py's crash record, so the next occurrence names the error code.

Rerun with the same shard lists: `socket_filesystem_test` and `socket_abstract_test` do not finish reliably in one kernel on the current head (the stall above); with the winstub change they should.

## Harness issues found on the way (all fixed)
Driver and runner heuristics gave up after six consecutive crashes and mislabelled 2341 cases; the culprit of a crash is now named by the driver's start line; streaming of the guest output every 500 ms lost the last cases before a crash; the Linux baseline job once hung after finishing (a runner problem, results were uploaded).

## Open problems
- The Sentry stops after hundreds of socket cases in one kernel (first row): the largest single source of Windows-only failures, and unlike the others not a missing feature.
- The cgroup, overlay, ACL, fuse and tun groups are the harness's missing mounts and devices, not necessarily the port's; they need `sentryguest` to mount them.
- Not run: a KVM or ptrace platform on Linux, upstream's per-test blocklists, save/restore variants.

Verdict: gVisor's syscall suite builds statically in 13 minutes and runs in CI under both runsc and the Windows Sentry, where 83.6% of the cases that pass on Linux pass (11298 of 14620, 138 of 256 binaries complete), and the largest remaining gaps are the Sentry stopping after a few hundred socket cases, ptrace, signal delivery to blocked tasks, and mounts the harness does not provide.
