# 1b. Sentry host basics: fault recovery, guest memory, startup

Nodes: "Port fault recovery to Windows" (sentry-fault-recovery), "Guest memory on Windows sections" (sentry-memory-file-sections), and the Linux-only assembly and calls that stopped the Sentry from reaching its init. Everything below ran on `windows-latest` (Windows Server 2025, 10.0.26100); nothing was run on the dev machine.

CI: https://github.com/lhns/gvisor-windows/actions/runs/45875421960 (workflow `.github/workflows/windows-sentry.yml`, green).

## What is implemented

### pkg/safecopy (vectored exception handler)
- `safecopy_windows_amd64.s`: a raw-assembly vectored exception handler, registered once in `init` as the first handler (`AddVectoredExceptionHandler(1, ...)`). For an access violation (signal 11) or in-page error (signal 7) whose RIP lies inside memcpy, memclr, the atomics or checkXstate, it sets RIP to that routine's existing `handle*Fault` stub, RAX to the fault address and RDI to the signal, the same contract as the Linux signal handler, so `safecopy_unsafe.go` is unchanged. Any other exception continues the search, so the Go runtime's own handler still gets it (checked: `debug.SetPanicOnFault` recovery still works afterwards).
- The handler uses only AX, CX, DX and R8-R11 (DI and SI are callee-saved in the Windows ABI).
- Windows clobbers R8 when it resumes a thread after a vectored handler returns `EXCEPTION_CONTINUE_EXECUTION` (it held the stack pointer at the stub). The memclr stub reads R8 and CX to tell whether REP STOSB faulted and where to restart, so with R8 wrong it restarted with garbage and zeroed random memory (hangs, `fatal error: bad ts`). The handler now passes R8 and CX through R12 and R13, which are restored, and `winMemclrFault` moves them back. Found by probing the stub entry in CI; no other stub reads R8.
- The Linux handler, its saved-handler variables and the install code moved unchanged to `safecopy_linux.go`; `sighandler_*.s` are `!windows`. `init` calls `installFaultHandlers()`.
- Cost: 1.3 us per recovered fault; 20000 repeated and 16000 concurrent (8 goroutines) faults all recovered.
- Not covered: SIGBUS. Windows reports in-page errors for I/O failures of file-backed views; there is no deterministic source of them, so the 288 SIGBUS subtests of the upstream tests are skipped on Windows.

### pkg/sentry/pgalloc (MemoryFile on sections)
- Host-specific code moved out of `pgalloc.go` into `pgalloc_host_linux.go` (code moved unchanged) and `pgalloc_host_windows.go`; `pgalloc.go` calls `hostInit`, `hostGrow`, `hostPrepare`, `hostDestroy`, `hostTotalUsage`, `hostCanPopulate`, and `chunkInfo` has a `section` field.
- Section type: one pagefile-backed section per 1 GB chunk, created `PAGE_READWRITE | SEC_RESERVE` and mapped once (`MapViewOfFile`) into the Sentry for `MapInternal`. SEC_COMMIT charges the whole section to the commit limit at creation, so 1 GB for the first allocation; SEC_RESERVE charges only what is committed.
- Growth: pgalloc already grows in 1 GB chunks; each new chunk creates and maps a new section, so there is no reserved maximum and no prototype-PTE table for terabytes. The Linux code's ftruncate-and-mmap becomes CreateFileMapping and MapViewOfFile. `Sections(fr)` returns the (section handle, offset in section, file offset, length) of each chunk a range touches, for the future platform's `MapViewOfFile3(MEM_REPLACE_PLACEHOLDER)` into stub processes (S2: 4 KB granularity inside the section works).
- Commit: touching an uncommitted page of a SEC_RESERVE view is an access violation, unlike the lazily faulted memfd. `Allocate` therefore commits the range (`VirtualAlloc(MEM_COMMIT)`) in every mode; commit is shared by all views of the section and cannot be undone for a view (`VirtualFree(MEM_DECOMMIT)` fails on views).
- Decommit (zero and release): `DiscardVirtualMemory` on the committed pages, then a check that they read as zero, else `clear`. The pages stay committed and accessible. The contents after a discard are undefined by the documentation, so the check is not optional; in CI every discard read as zero (1154 MB discarded, 0 MB cleared, 0 non-zero). It never fails, so `DecommitOnDestroy` cannot panic.
- Not implemented: `File()`/`FD()` are nil/-1 (no fd), `NewMemoryFile` ignores and closes the file it is given (nil is fine), save/restore (it needs fds), `MADV_POPULATE`, huge pages. `mincore` is `QueryWorkingSetEx` (resident pages), so `UpdateUsage` and `TotalUsage` work on resident, not committed, pages.

### Startup
Found by running each package's init in CI; each fixed with a `_windows.go` file or a marked edit:
- `pkg/sentry/time`: `vdsoClockGettime` called the Linux VDSO in assembly; Windows version in Go (`GetSystemTimePreciseAsFileTime`, `QueryPerformanceCounter`) for the Sentry's own clocks. The asm and `vdso.go` are `!windows`.
- `pkg/abi/linux`: `TaskSize` probed with mmap; fixed to 0x7ffffffff000 on Windows.
- `pkg/sentry/platform`: reads `/proc/sys/vm/mmap_min_addr`; 0x10000 on Windows (S0: nothing can be mapped lower).
- `pkg/sentry/fsimpl/gofer`: init creates a host pipe; skipped on Windows (host files are replaced, ADR 0005).
- `runsc/boot`: the MemoryFile's memfd is created per OS (`memfile_linux.go`, `memfile_windows.go` returns nil).
- Already fine on Windows: `pkg/rand` (crypto/rand), `pkg/cpuid` (cpuid asm works; HWCap and /proc/cpuinfo only warn), `pkg/hosttid` (the assembly reading `getg().m.procid` equals `GetCurrentThreadId`), `pkg/hostarch`.

## Tests (all on windows-latest)
- `pkg/safecopy`: the upstream tests copied from gVisor commit c3efd8c93 (the `go` branch's base): 29 top-level tests passed, 0 failed, 3 skipped; subtests 292 passed, 0 failed, 288 skipped (the SIGBUS cases above). Adaptations: `safecopy_amd64_test.go` is package `safecopy_test` instead of `safecopy_x_test` (only Bazel accepts the latter), and the `unix.Mmap`/`Mprotect`/`Memfd` uses went into per-OS helpers (`testmap_linux_test.go`, `testmap_windows_test.go`). New `faultmatrix_windows_test.go` checks ZeroOut at 11 sizes x 4 offsets x REP STOSB on and off.
- `pkg/sentry/pgalloc`: the upstream allocator tests pass unchanged; the two tests that need a memfd or disk-backed file (`zeroDiskBacked`, in `pgalloc_diskbacked_linux_test.go`) and `save_restore_test.go` are Linux-only. New `pgalloc_windows_test.go`: allocation in every mode, write/read/Decommit through MapInternal and through a second view of the section, growth across a chunk boundary, `TotalUsage`. 5 top-level tests passed, 0 failed.
- `tools/windows/sentrysmoke`: PASS for rand, hosttid, cpuid, clocks (monotonic advanced 20.5 ms over a 20 ms sleep, realtime within 0.2 ms of `time.Now`), all safecopy cases (copy in/out/zero/swap/CAS/load faults with the right address, 20000 repeated and 16000 concurrent), the Go runtime's own fault recovery next to it, and the MemoryFile (256 MB write/read, second view sees writes, Decommit zeroes in both views, recycled pages read zero).
- Init of 11 packages alone (hosttid, cpuid, hostarch, rand, safecopy, sentry/time, pgalloc, kernel, vfs, loader, runsc/boot): all PASS. `runsc/boot` links and initialises; nothing in it was run beyond init.

## Does Decommit give memory back?
Partly. With one view of the section, touching 512 MB grew the working set 525 to 1037 MB and available physical memory fell 512 MB; after Decommit the working set was 779 MB (258 MB released) and available physical memory had regained 279 MB. With a second view mapped (as a stub process will have) the working set did not change, since the other view keeps the pages mapped. Commit charge is not reduced at all (the pages stay committed). So Decommit is correct (zeroes) but not yet a memory-return mechanism; measure again when real stubs map the views.

## Open problems
- Commit charge only grows: freed pages stay committed. A real release needs a per-view decommit, which Windows does not offer; options are sections per smaller unit (e.g. 64 MB) that are closed when empty, or accepting the charge.
- Only half of the touched pages left the working set after Decommit (see above); not investigated further.
- Windows' R8 clobbering on resume is empirical; if other stubs ever read volatile registers they need the same treatment.
- Exception dispatch runs on the goroutine stack. Nothing overflowed in CI, but a goroutine near its stack limit with an AVX-512 context could; `ensureFaultStack`-style headroom (a frame-only assembly function before each asm call) was prototyped and dropped because the failures it was meant for had another cause.
- Not tried: `go vet` on Windows (unsafe.Pointer warnings of the upstream tests), arm64, the Linux tests of pgalloc and safecopy (`GOOS=linux go build ./runsc/...` and test binaries compile; they were not run, since they need a Linux runner).
- CI took 9 rounds, not 5: rounds 1-2 hit startup panics in init, rounds 3-7 chased the memclr failure (a stack-size theory came first and was wrong; probing register values in the handler found the R8 clobbering).

Verdict: pkg/safecopy recovers faults on Windows through a vectored exception handler (1.3 us per fault, 292 upstream subtests passing) and pgalloc's MemoryFile runs on SEC_RESERVE sections with a handle accessor for later stub mappings, and the Sentry's packages, up to `runsc/boot`, now initialise on Windows; releasing committed memory is still open.
