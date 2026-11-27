# 1r: platform speed (fork+exec, vDSO)

Verdict: the 100x fork+exec loop through `runsc.exe` went from 2.0 to 3.6 s to 0.4 to 0.6 s in all three legs, past the 0.87 s target (4x faster than 3.47 s); the causes were first-touch faults, patching the same text again in every process, and creating a stub process and a stub thread per address space. `clock_gettime`, `gettimeofday` and `time` already ran in the vDSO without a trap, and `getcpu` now does too.

Nodes: `platform-recycle-stubs`, `platform-own-vdso`. Design: ADR 0021.

## What a fork+exec cost

Measured with `WINSTUB_TRACE` (the trace now prints the kind and address of every access violation, the view it hit and the time of every `MapFile` and text patch), on the development VM (`runsc.exe`, rewrite + fxsave) and in `windows-profile` (`sentryguest`, native):

- **Faults**: 25 to 36 access violations per iteration (356 in 10 iterations on the VM, 262 in CI). They were not `%fs` repairs, W^X flips or Sentry pmas missing: every one was the first touch of a pma. The fork child touched the shell's data, bss, heap and stack (each read first, then written: two faults for one copy-on-write), its text and rodata; the exec'd child its text, ELF header, rodata, data, bss, `brk` heap, vDSO and stack; the parent took 2 to 3 copy-on-write faults after the child exited; glibc's RELRO `mprotect` unmapped data that was then touched again. Each is a debugger round trip plus a `MapFile`: 0.1 ms native in CI, 0.24 ms average through `runsc.exe`, 0.3 to 0.4 ms on the VM.
- **Text**: both the fork child and the exec'd child copied, hashed and patched the 1.5 to 1.8 MB busybox text: 2 ms native in CI, of which 0.9 ms was the SHA-256 of the scan cache key; on the VM 27 to 37 ms, 13 to 18 ms of it SHA-256 (no SHA instructions) and 12 to 16 ms placing 3900 `%fs` veneers.
- **Stub processes**: two per iteration, 5 to 10 ms each in CI (`CreateProcess`, the loader's DLL events, the anchor thread). The address-space pool of `runsc.exe` hides them only while it keeps up; `WINSTUB_SPARES=2` started to help once the rest got faster. On the VM about every eighth `CreateProcess` took 400 ms (not seen in CI).
- **Stub threads**: one `CreateRemoteThread` per address space, then ntdll's thread start-up under the debugger and a 1 ms polling sleep until it runs: 1.5 to 3 ms per address space. This was the largest part left once the others were gone.

## Changes

| change | where | what it does |
|--|--|--|
| eager mapping | `pkg/sentry/mm/eager.go`, `lifecycle.go`, `syscalls.go`, `loader.go` (marked `gvisor-windows`, set by winstub) | fork copies the parent's private memory for the child when it is at most 8 MiB (no copy-on-write on either side), shares the parent's other translations (text, rodata) and maps all of it into the child's stub at once; execve maps every mapping of the new image with the access it allows (writable segments copied at once) and the stack from the stack pointer up; `brk` growth up to 2 MiB is populated like a small anonymous `mmap`; `mprotect` maps the range again with its new permissions instead of leaving it to fault. `WINSTUB_EAGER=0` turns it off. |
| scan cache key | `patch_windows.go` | `maphash` finds candidates, an exact comparison with the kept original decides; no SHA-256 of the whole text. |
| patched-text cache | `textcache_windows.go` | the result of patching a piece (patched section, stub area, tables, restart and boundary state) is kept under the history of text operations of the address space and the address range, checked against the original bytes, and taken by a later address space with the same history: a map of the cached sections instead of a copy, scan and patch. Sections are shared read-only and copied before an in-place repatch (reseam). The state is kept from the second identical patch on, so text seen once (a PIE or a dynamic binary, at a random address) costs only its bytes (bounded at 32 MiB, the rest at 128 MiB). `WINSTUB_TEXTCACHE=0`. |
| stub recycling | `recycle_windows.go` | a released address space whose threads all wait for a reply is emptied in the background (views unmapped, patch state of a new space, the guest range checked to hold no committed page but the stub's own) and handed out by the next `NewAddressSpace`; up to 4 are kept. A generation number keeps a context that still holds a thread of the old owner from touching it. `WINSTUB_RECYCLE=0`. |
| thread reuse | `process_windows.go` | a freed slot keeps its Windows thread, waiting for a reply; the next context of the address space (or of the next owner of a recycled stub) gets it, and its first reply restores the whole context. `WINSTUB_THREADREUSE=0`. |
| getcpu | `stubasm.go`, `patch_windows.go`, `context_windows.go` | the vDSO's `getcpu` (a bare `syscall`) is patched into a stub routine that stores the task's CPU number, written into the thread page on every resume, and node 0. |

Tests: `TestTextCache` (an upper piece mapped before the lower one in four address spaces: two patch it themselves, two take the cached patch including the reseam; all run their syscalls, and a fifth that maps only the upper piece gets it as it was before the reseam), `TestRecycle` (a released space comes back empty with a new generation, runs other code, and the old owner's context releasing its thread does not touch the new owner's thread in the same slot), both in windows-platform in all legs. The guest `test/guests/vdso.c` and the `vDSO without traps` step of windows-sentry-guests (below). `windows-stability` now also times the loop with each change off in turn.

`sh_wait_status` (windows-sentry-guests) expected the killed background job to be reported as `Terminated`; with faster fork+exec busybox sometimes reaps it before `wait` and prints `/t/wait.sh: Terminated`. Both are ash; the script now hides that report.

## Numbers

`windows-stability`, "Fork+exec time": median of 7 runs of `runsctest -only forkexec` (100 times `/bin/true` from busybox `sh` through `runsc.exe`, boot included: `noop` is 0.11 to 0.24 s), without and with `WINSTUB_SPARES=2`, seconds:

| code | native | rewrite | rewrite-fxsave | run |
|--|--|--|--|--|
| before (dfd89b2a3, tracing only) | 2.07 / 1.96 | 3.34 / 3.31 | 3.56 / 3.23 | 46050837690 |
| eager mapping, scan key, getcpu (35296e553) | 1.31 / 1.30 | 2.39 / 2.41 | 2.75 / 2.70 | 46052146164 |
| + patched-text cache (1c87480ce) | 1.11 / 1.01 | 1.33 / 1.10 | 1.87 / 1.44 | 46053737295 |
| + stub recycling (e4df952bb) | 1.01 / 1.02 | 0.99 / 1.00 | 1.11 / 1.10 | 46055898979 |
| + thread reuse (b2950c6e7) | 0.37 / 0.36 | 0.58 / 0.57 | 0.45 / 0.46 | 46056539669 |
| same code, another runner | 0.58 / 0.63 | 0.45 / 0.46 | 0.57 / 0.56 | 46056850721 |
| final (b1644369c) | 0.44 / 0.36 | 0.57 / 0.58 | 0.57 / 0.56 | 46058080724 |

Target 0.87 s: met in every leg. Faults per 10 iterations on the VM: 356, then 63 with the fork and exec mapping, 0 with `brk` and `mprotect` too. On the VM the 100 iterations took 10.5 s before and 1.2 s after (boot included).

The same run with each change off in turn (median of 7, seconds; all on / eager mapping off / text cache off / recycling off / thread reuse off / all off):

| leg | all on | eager off | text cache off | recycling off | thread reuse off | all off |
|--|--|--|--|--|--|--|
| native | 0.36 | 1.07 | 0.77 | 1.13 | 0.46 | 1.32 |
| rewrite | 0.56 | 2.57 | 2.05 | 1.54 | 0.56 | 3.07 |
| rewrite-fxsave | 0.56 | 2.48 | 1.96 | 1.53 | 0.57 | 3.03 |

Each part matters; thread reuse off changes little because a recycled stub keeps its threads anyway (reset leaves them waiting for the next owner), so the switch only covers threads freed within one address space. "All off" is not the old code: the scan-cache key and getcpu stay.

windows-profile run 46058129875 (same code): `runsc.exe` forkexec 0.44 to 0.45 s, also with the address-space pool off (`GVISOR_ASPOOL=0`: 2.43 s before); `sentryguest` 100x fork+exec 0.37 s without the pool (2.36 to 2.49 s before) and 0.35 s with 2 spares (1.33 to 1.48 s before); 0 access violations in the 10-iteration trace.

Single process start (windows-sentry-guests "Timing", native, median of 9): hello_asm 15.1 ms, static glibc hello 30.8 ms, fork + waitpid 38.7 ms, Go hello 41.8 ms (run 46056843847; 15.4, 39.0, 46.8 and 49.7 ms in run 46016206784 before this work): the eager mapping does not slow a start down.

## vDSO

How it works today: the loader maps the Sentry's vDSO and its parameter page; the page is memory-file memory, so the stub maps the Sentry's own pages and sees every update of the timekeeper. The vDSO's `clock_gettime`, `gettimeofday` and `time` read the TSC and the parameters (seqcount, base cycles, base time, frequency) and never trap once the Sentry's calibrated clocks are ready (about a second after start; until then the parameters say not ready and the vDSO makes the system call). Its `getcpu` is `mov $309,%eax; syscall; cltq; ret`: always a system call. Now that exact function is patched to a stub routine (`stubasm.GetcpuCode`) that returns the CPU number of the task (`Task.CPU`, what the `getcpu` system call returns), which `Switch` writes into the thread page; other `getcpu` syscall sites keep the system call (its `EFAULT` for a bad pointer).

TSC: the parameters are those the Sentry uses for its own `clock_gettime`, calibrated against the performance counter, so the vDSO is exactly as good as the system call; if the calibration fails or drifts (`CalibratedClock` resets on an error over a quarter of the update interval) the parameters are marked not ready and the vDSO falls back to the system call by itself. No separate fallback was needed: CI runners report invariant TSC, the development VM (QEMU) does not, and both pass.

`test/guests/vdso.c` finds the four functions in the vDSO (`AT_SYSINFO_EHDR`), waits 2.5 s for the calibration, makes 1e6 `clock_gettime` calls (both clocks), compares the clocks with the system call 2000 times (a vDSO read must not be ahead of a later system call read; 0 ns in every run), and makes 1e5 `gettimeofday`, `time` and `getcpu` calls with value checks. The `vDSO without traps` step of windows-sentry-guests runs it with `-profstats` in all legs and fails if the Sentry saw more calls of any of the four than the guest made directly (and more than 50 other system calls in all): 2002, 1000, 1 and 0 of 2002, 1000, 1 and 0 in every leg (run 46052149949), 26 to 30 ns per `clock_gettime` in CI and 38 ns on the VM.

## Checks on the final code

Product code at b1644369c (d00d8e206 changes only test/guests/t/wait.sh):

| check | result |
|--|--|
| windows-platform, all legs (with TestTextCache, TestRecycle) | green, run 46058068388 |
| windows-sentry-guests, all legs (vDSO step included) | green, run 46059096163 (d00d8e206) |
| windows-runsc, all legs | green, run 46058076777 |
| windows-stability, 300 runs per leg | 300 of 300 in native, rewrite and rewrite-fxsave, run 46058080724 |
| podman-windows modes=native,rewrite,rewrite-fxsave runs=4 | all 10 steps in all 12 runs, run 46058084568 |
| development VM (rewrite + fxsave), the sentry-guests cases through runsc.exe | all pass (sh_wait_status differs only in stderr order) |

## Open

- PIE and dynamic executables do not hit the text cache (random load addresses); they gain the fault, stub and thread parts. Loading them at a fixed address per file under winstub would let them hit too.
- A recycled stub keeps its previous owner's Windows-side state (ADR 0021); the boundary is the Sentry.
- The development VM's 400 ms `CreateProcess` stalls were not diagnosed; with recycling they only hit the first stubs.
