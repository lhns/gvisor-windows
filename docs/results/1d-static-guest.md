# Phase 1d: FP state, static guests under the real Sentry, prototype retired

Nodes: "Save and restore FPU and SSE state" (platform-fp-state), "Run a static guest from memory" (platform-static-guest-from-memory), "Retire the stub prototype" (platform-retire-prototype). Everything ran on `windows-latest`; nothing was run on the dev machine.

CI: platform tests https://github.com/lhns/gvisor-windows/actions/runs/45879823120 (`windows-platform.yml`, 7 tests pass incl. `TestFPState`); Sentry guests https://github.com/lhns/gvisor-windows/actions/runs/45882018323 (`windows-sentry-guests.yml`, all 8 guests pass).

## FP, SSE and AVX state
- The stub saves and restores the state itself with `xsave64`/`xrstor64` (standard format, the layout of the Sentry's `fpu.State`) in a 4 KB area at offset 0x1000 of each thread region (region 32 KB, `MaxThreads` 128). The mask is XCR0 without AMX, read once by the platform.
- Syscalls: the trampoline saves after the general registers and before it publishes the request; the reply path restores before the registers. Faults, interrupts and `ud2` syscalls: the debugger only stores the general registers and redirects the thread to a new stub routine (`trampPark`), which saves the vector state, publishes the request and waits (no `CONTEXT_XSTATE` calls; the vector state is never touched by the debugger). `threadEntry` restores the Sentry's state for a new thread, so a guest starts with the Linux x87/MXCSR defaults, not Windows'.
- Sentry side: like systrap, the state is copied lazily: `PullFullState` copies page to `ac` (signal frames), `FullStateChanged` marks `ac` as newer, and the next resume copies it back; otherwise the thread keeps its own.
- Test `TestFPState`: ymm0 loaded by the guest survives a syscall, a Sentry-side change of xmm0's low half (the upper half of ymm0 stays), a fault with a `MapFile` in between, and an interrupt with a changed rip. Cost: the 4-thread syscall round trip went from 17 to 29 us in the platform test (xsave/xrstor, noisy runner).

## The Sentry runs guests
`tools/windows/sentryguest` is a minimal boot path (not `runsc/boot`: its Loader needs a spec, gofer, seccomp and controller): it creates the winstub platform, the Windows memory file, the VDSO, timekeeper, namespaces and `kernel.Kernel` as `runsc/boot.New` does, mounts an in-memory tmpfs as root, writes the guest binary and three stdio files into it (no host files), creates the process through `kernel.CreateProcess` (the real ELF loader, which maps text with `MapFile` and execute permission) and waits for it. Stdout and stderr are a tmpfs file read back after the exit.

Fixes the boot needed (marked `gvisor-windows:`): `pkg/sentry/usage` no longer needs a memfd on Windows (stats in heap memory, `usage_host_*.go`); `pkg/cpuid` skips `/proc/cpuinfo` and the auxv HWCap on Windows (the warnings are gone; CPUID is read directly as before); the harness imports the syscall table and passes the cgroup2fs initializer. The kernel, VDSO, tmpfs, ELF loader, vfs, futex, clone and fork code ran unchanged.

| guest | result |
|---|---|
| asm hello (patched write, `ud2`-path write) | PASS, output and exit 0 |
| static glibc hello (TLS, stack protector, malloc via brk and mmap) | PASS, output, exit 7 |
| static-PIE glibc | PASS |
| glibc fork + waitpid | PASS (`child`, `parent: child exited 3`) |
| static Go hello (threads, futex, signals) | PASS |
| glibc JIT test (RW then RX, RWX rewritten, register-number syscalls) | PASS |
| asm bench loops | PASS |

## Numbers (CI run above, 4 vCPUs, noisy: runs of the same guest differed by up to 1.3x between runs of this series)
- Platform and kernel initialization before the guest: 2-5 ms.
- A process, from `CreateProcess` to exit, through the whole Sentry (kernel start, ELF load, VDSO map, stub start, a few syscalls): 280-360 ms for the asm hello, 330-430 ms for glibc hello, 330-560 ms for the Go hello. The prototype alone took 18 ms; where the other ~250 ms go (kernel start, timers, the first stub, 30 mapped ranges, scanning) was not profiled.
- fork + waitpid in glibc: +260 to +410 ms over the plain glibc hello (a second stub plus remapping the child's pages through `MapFile`).
- getpid through the full Sentry path: 300000 getpid calls cost 160-240 ms more than the asm hello, so below 1 us per call at this resolution (patched site, spinning hand-off, Sentry syscall dispatch). The earlier 17-29 us figure is the platform test with four contexts competing for four vCPUs.

## The prototype is gone
`tools/windows/stubproto` and `windows-stubproto.yml` are deleted; the guest sources are `test/guests/` (the Go guest has its own module so the sparse checkout builds it). The stub code exists once, in `pkg/sentry/platform/winstub`. The Phase 1a and 1b results documents describe the prototype and keep their paths.

## What busybox still needs
- A real `runsc` boot on Windows (`runsc/boot.Loader`, spec, no gofer, seccomp off, control server), instead of this harness.
- Host files through the directfs backend (the harness has no host files; stdio is a tmpfs file), a terminal or pipe for stdio, `/proc`, `/dev` and `/sys` mounts (guests here need none).
- Dynamic ELF (an interpreter and shared libraries: patching of library text is in place, the `MapFile` of many small ranges is untested), `execve` of a second binary inside the guest (the context moves to a new stub; the path ran only in platform tests), pipes between processes, `wait` of several children, more than one thread group at a time.
- Signals beyond `exit`: delivery and `sigreturn` use the full-context resume path (platform tests cover it, the Go guest runs its signal handlers), but no guest here sends itself a signal and handles it. Ptrace and `arch_prctl(SET_GS)` are unsupported by design.
- Start-up cost (about 300 ms per process) and fork cost need profiling before shell-heavy workloads are usable; a pool of ready stubs is the obvious first step.
- Memory: the committed section pages are not returned (ADR 0013), and a `syscall` instruction split across two `MapFile` ranges is not patched.

Verdict: PASS. The real Sentry on a hosted Windows runner runs static asm, glibc (static and PIE), fork, Go and JIT guests from an in-memory tmpfs with correct output and exit codes, the stubs keep the guests' vector state across every kind of stop, and the Phase 1a prototype is deleted.
