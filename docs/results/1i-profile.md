# 1i. Profile of the slow small-file and exec path

Node: "Profile the slow file and exec path" (sentry-profile-small-file-path). Everything ran on `windows-latest` (`windows-profile.yml`, branch `sentry-profile`, nothing on the dev machine). Final CI run: https://github.com/lhns/gvisor-windows/actions/runs/45923754536 (earlier runs of the same workflow: 45914785752, 45917004854, 45918242442, 45919351478, 45920385125, 45921185943, 45921673735, 45922463134).

## How it was measured
- `GVISOR_PROFSTATS=1` (new, `pkg/sentry/kernel/profstats.go`, `pkg/sentry/profkeys`): per syscall number the calls and the time inside the Sentry's implementation, the time between a resume and the return of `Platform.Switch` by outcome (syscall, fault, interrupt), and free-form counters (openat by filesystem type, overlay lookups by name). `runsc.exe` prints them to stderr at exit, `sentryguest -profstats` to stdout.
- `RUNSC_CPUPROFILE=file` (runsc.exe) and `sentryguest -cpuprofile=file`: Go CPU profile of the Sentry process, printed with `go tool pprof -top` in the job (`runsc-pprof.txt`, `pprof.txt` in the artifacts).
- `WINSTUB_TRACE=1` with `sentryguest` on a 10-iteration fork+exec loop and a small untar (`trace-*.txt`).
- Workloads: `untar` is the Phase 0 tarball (20k files of 512 to 8192 bytes in 2000 directories, built as in `windows-runsc.yml`) extracted by busybox `tar` onto the container's tmpfs `/tmp`; `forkexec` is 100 times `/bin/true` from a busybox `sh` loop; `noop` is `/bin/true` once (start-up 0.14 to 0.25 s). Through `runsc.exe run` (as in 3b) and, for the platform part, through the in-process harness `sentryguest` (tmpfs root, no runsc).

## Untar: the cause was not the platform
In `sentryguest` (tmpfs root) the 20k-file untar takes 0.7 to 0.8 s (the syscall round trip is cheap, as measured before), but through `runsc.exe` it took 8 to 22 s (the 20.9 s of 3b; the runner differs by 2.5x from run to run). So the cost was in what `runsc.exe` adds.

Per-syscall counters of the untar through `runsc.exe`, before the fix (run 45914785752):

| syscall | calls | total ms | us/call |
|---|---|---|---|
| openat | 68003 | 12420 | 182.6 |
| sendfile | 20000 | 888 | 44.4 |
| read | 24023 | 840 | 35.0 |
| mkdir | 68004 | 102 | 1.5 |
| newfstatat | 64002 | 92 | 1.4 |
| chown, chmod, utimensat, close, unlink | 20000 to 24000 each | 26 to 78 each | 1.3 to 3.3 |
| all (420059 calls) | | 14606 | |

The guest side (`Switch` returns of 420059 syscalls) cost 1.65 s together, so the Sentry's own implementation was 88 percent of the 16.7 s. `openat` by outcome (`openat <fs> abs|rel creat err`): 20000 creates on tmpfs for 45 ms, and **48002 failed opens of absolute paths for 6.3 s** (130 us each). They are busybox `tar` (running as root) looking up user and group names for every file: `open("/etc/passwd")` and `open("/etc/group")`, which do not exist in the container. The Go profile agreed (`overlay.(*filesystem).lookupLocked` 25 percent, `gofer.revalidate` 21 percent, `winhost.ntOpenUS`/`NtCreateFile` 17 percent): the path goes through the root overlay, and the overlay drops a dentry that nobody references, so every walk of `/etc` looks it up again in the lower layer, the host folder of the rootfs, which was mounted with revalidation on every use: one `NtCreateFile`, query and close per path component (about 130 us), then the failing lookup of `passwd` itself. Per-name overlay lookup counters confirmed it: `etc` in `/`: 48002 lookups, 5.95 s; `passwd` and `group` in `etc`: 24001 each, 0.57 s each. With a tmpfs root the same failed open costs 0.7 us.

One fix attempt that did not work: an overlay negative-lookup cache (a name on no layer stays absent until the directory changes) did nothing, because the parent dentry itself does not survive the walk. It was removed again.

Fix: the root folder of a container is mounted with `cache=exclusive` by default (`runsc/boot/hostfolder_windows.go`; the annotation `dev.gvisor.windows.rootfs-options` can say `cache=shared` to get the old behavior back). The Sentry then trusts its own cache of the root folder and does not revalidate every walk; below the root overlay the folder is read-only anyway, and the Sentry is its only user. Bind mounts keep revalidation (`cache=exclusive` and `cache=watched` per mount as before).

After the fix (run 45919351478): `openat` 68003 calls, 358 ms (5.3 us), `sendfile` 312 ms (15.6 us), all 420059 calls 996 ms; the untar takes **1.7 to 2.3 s through runsc.exe, 8 to 22 s before** (same job, same code, old default `cache=shared`: 22.1 s, 12.3 s and 12.9 s in runs 45920385125, 45923754536 and 45921185943; the runner swings, the new value does not: 1.7 to 2.3 s in 12 runs). That is 6x to 12x, and 9x against the 20.9 s of 3b. What is left is 1 s of Sentry implementation (sendfile from the tar file on the host-folder lower layer, the creates) and about 1 s of guest and platform time, roughly 2.5x of the in-process harness.

Debug events per file: none worth counting. The whole untar causes 122 to 125 fault returns (22 to 24 ms in total) and one interrupt, because the guest touches few pages and `mm` maps a whole pma per fault. No `ud2` site, no W^X rescan and no `%fs` repair occurs in this workload.

## fork+exec: two address spaces per iteration
`forkexec` is 33 to 36 ms per iteration through `runsc.exe` (3.3 to 3.6 s for 100; 3b: 32 ms) and 24 to 29 ms in `sentryguest`. Per-syscall counters (100 iterations): `clone` 7.6 to 8.4 ms per call and `wait4` (the parent waiting for the child) 5.6 to 7.6 ms; everything else in the Sentry's implementations is below 120 us (`execve` 10 us, `mprotect` 33 us, `readlinkat` 118 us). So the Sentry's own work is negligible; the time is the address spaces:

- `mm.NewMemoryManager` (execve) and `mm.Fork` (clone) each call `Platform.NewAddressSpace` synchronously, and for winstub that starts a host process. From the winstub trace, one address space start takes 10 to 11 ms: `CreateProcess` 5.4 ms, placeholders, sections and views 0.4 ms, then 3 loader debug events (ntdll, kernel32, KernelBase) and the anchor thread 5 ms. Per iteration there are two of them (10 iterations made 21 address spaces) plus `as: release` and `process gone` 1 ms each.
- 12.5 access-violation exceptions per address space (262 for 21), each a debug event, a Sentry `MapFile` and a resume: about 0.4 ms each, 5 ms per address space. The Sentry's counters show 6152 to 6452 fault returns costing 1.08 to 1.13 s for 100 iterations (60 to 65 per iteration, the shell's and the child's). Plus 7 other debug events per address space (process, thread and DLL events).
- The Go profile is 88 percent `debugLoop` (`WaitForDebugEvent` and `ContinueDebugEvent`) and process creation time, 2 to 4 percent Sentry code.

Change in the Sentry (no edit of `winstub`): `platform.NewASPool(p, n)` (`pkg/sentry/platform/aspool.go`, used by `runsc/boot/loader.go` for winstub with 2 spares, `GVISOR_ASPOOL=0` turns it off, `sentryguest -aspool=N`) keeps address spaces started in advance and refills in the background, so fork and execve take a ready one. Result (same jobs): `runsc.exe` forkexec 3.47 s to 2.01 s (1.7x), `sentryguest` 2.43 s to 1.17 s (2.1x; 4 spares: 1.25 s). A fork+exec is 20 ms through runsc and 11.7 ms in the harness now. **The 4x is not reached:** the floor is the throughput of creating address spaces (about 6 ms each on the 4 vCPUs, so a spare is rarely ready for the second request of an iteration) plus the 25 faults per iteration.

## What the platform owner needs to change (not edited)
In order of effect for fork+exec (33 ms now):
1. **Recycle stub processes instead of creating one per address space.** `as: release` kills the process and `start` creates a new one (CreateProcess 5.4 ms plus the loader events 5 ms). An `execve` replaces the whole address space of a task that has a stub already; the released stub could be unmapped, its trap table and veneer area reset, and handed to the next `NewAddressSpace`. That removes both the 10 ms and the CPU the pool needs, and should bring a fork+exec to about 12 ms (25 faults, 10 ms) without the pool, and below that with it.
2. **Fewer faults per address space.** 12.5 access violations per start are mostly first touches of text, data, bss, stack, heap and vdso; each costs 0.4 ms. Mapping the ranges the Sentry already has as pmas up front (at the first `Switch` of a new address space, or in `PostFork` for a forked child whose pmas are copied) would turn about 12 events into a few calls.
3. **`as: start` itself:** the three module-load debug events (ntdll, kernel32, KernelBase) and the anchor thread take 5 ms; a stub image that does not need them, or acknowledging them without a Sentry round trip, would shorten it.

## Open: a hang in the fork+exec loop
`runsc.exe` hangs in the 100-times fork+exec loop at a rate that depends on the root folder cache mode: 9 of 48 runs with `cache=exclusive` (the new default), 2 of 48 with `cache=shared` (runs 45921185943, 45921673735, 45922463134, 45923754536, 8 to 16 runs of each mode per job). So the faster root folder makes an existing race more likely, it did not create it (the shared hangs also happened). The goroutine dump of a hang (`out/hang/dump-exclusive-12.txt` of run 45923754536, written by `RUNSC_HANG_DUMP=file:seconds`) shows the Sentry idle: the shell's task blocked in `wait4`, no other task goroutine, four `debugLoop` goroutines (the shell's address space, two spares of the pool and one more), the kernel waiting for the exit. A child address space exists but no child task: either an exit notification to the waiting parent is lost or a child's creation stalls after `NewAddressSpace`. Not diagnosed further; the platform owner's hang hunt is the likely home of it. Until then `cache=shared` for the root folder is the safe setting for fork-heavy work.

## Numbers
| | before | after | factor |
|---|---|---|---|
| untar 20k files, tmpfs, runsc.exe | 20.9 s (3b); 12 to 22 s in the same job as the after-runs | 1.7 to 2.3 s | 6x to 12x |
| untar, `sentryguest` (tmpfs root) | 0.7 to 0.8 s | same | |
| fork+exec of /bin/true, runsc.exe | 33 to 36 ms | 20 ms | 1.7x |
| fork+exec, `sentryguest` | 24 to 29 ms | 11.7 ms | 2.1x |
| noop (`runsc run` of /bin/true) | 0.24 s | 0.14 to 0.25 s | |

Other: the branch history contains a 57 MB `runsc.exe` committed by mistake in commit 334c4a747 and removed in the next commit (a force push was refused); merge it squashed.

Verdict: the untar was slow because every failed `open("/etc/passwd")` walked the root overlay and revalidated the host folder (130 us, 48000 times); trusting the Sentry's cache of the root folder made it 6x to 12x faster, while fork+exec is dominated by two host-process starts and 25 debug-event faults per iteration, which the address-space pool only halves (1.7x to 2.1x) and which needs stub recycling and fewer faults in `winstub` to get past 4x.
