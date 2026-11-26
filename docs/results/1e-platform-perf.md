# Phase 1e: platform performance, split syscalls, robustness

Nodes: "Start guest processes fast" (platform-fast-process-start), defect "Syscalls split across mappings" (platform-split-syscall-unpatched) and the two robustness tests. Everything ran on `windows-latest` (4 vCPUs, noisy: single runs vary by up to 1.5x); nothing was run on the dev machine.

CI: platform tests https://github.com/lhns/gvisor-windows/actions/runs/45890490763 (`windows-platform.yml`), Sentry guests and timings https://github.com/lhns/gvisor-windows/actions/runs/45890492323 (`windows-sentry-guests.yml`).

## Profile of a start (`WINSTUB_TRACE=1`, and `sentryguest -sample`)
Trace points: stub created, setup phases, every debug event with the DLL name, first request, release, boot phases of `sentryguest`, plus a 2 ms sampler of the goroutine inside `kernel.CreateProcess`. Where the 280-360 ms of the asm hello (and ~600 ms of fork) went:

| cost | where | size |
|---|---|---|
| Windows loader stuck in a long search | with the whole range [0x10000, 0x7000_0000_0000) reserved as placeholders, ntdll's process initialization (between the ntdll and kernel32 loads, see the DLL names in the trace) allocates from the lowest free addresses and finds none; it falls back after 100-250 ms. The prototype had a 1 GiB free window at 0x7F0_0000_0000 that I removed in 1c as unneeded | 100-250 ms per stub |
| `log.Infof` in `kernel.CreateProcess` | the log emitter formats the time in the local zone; the first use costs ~30 ms on Windows (registry reads) | 30 ms |
| scanning the text | linear x86 decode of 700 KB of glibc at ~40 ns per instruction, once per process (the fork child scanned it again) | 30-50 ms per process |
| per-stub setup | CreateProcess 10 ms, placeholders, sections and views 0.5 ms, the anchor thread parked after 1 ms | 5-8 ms |

Not a cost: the 128 thread regions (one section, mapped once), the hole reservations, the anchor thread, debugger attach and process teardown (release 0-0.6 ms). Experiments (`TestStubStartExperiment`, CI): stub start to loader breakpoint 258-267 ms with no hole, 6-10 ms with a free window of 4 MiB or more at 0x2_0000_0000, 125-190 ms with 64 KiB to 1 MiB, 25 ms for the prototype's 1 GiB window at 0x7F0_0000_0000, and 250 ms for a window just above the guest range (so position matters, not only size).

## Fixes
1. A free 64 MiB window at 0x2_0000_0000 (`stubasm.WinWindow`) left out of the placeholders; it is a hole in the guest range, reserved in the memory manager (sealed, no access) together with the 0x7FFE0000 hole. 4 MiB is the least that works; the rest leaves room for thread stacks and TEBs. Stub start (create to anchor parked): 130-260 ms to 4.5-8 ms (`TestStubStart`).
2. Log timestamps in UTC on Windows (`pkg/log/glog.go`, marked `gvisor-windows:`): 30 ms off every process creation.
3. The scan is cached by content (SHA-256 of the range, per platform): the result is the syscall sites with the three instructions on each side and which of them are branch targets, independent of the mapping address, so a fork child, a second process or a second mapping of the same library does not decode again. The cold decode runs in up to four chunks in parallel (a chunk is decoded from an arbitrary offset and joined where its instruction stream meets the true one; `TestDecodeAllMatchesSerial` compares the result with the serial sweep on 6 MB of real code: identical).

## Before and after (`run` = from `CreateProcess` through exit, whole Sentry; median of 9 runs after)
| guest | before (1d) | after |
|---|---|---|
| asm hello | 280-360 ms | 16.1 ms |
| static glibc hello | 330-430 ms | 41.6 ms |
| glibc fork + waitpid | 590-840 ms (adds 260-410 ms) | 51.5 ms (adds about 10 ms: the child's stub costs ~5 ms and its scan is cached) |
| Go hello | 330-560 ms | 51.5 ms |

Targets: glibc hello below 50 ms: met (41.6); fork + waitpid adds below 50 ms: met. No stub pool was needed (it would only hide the 5 ms). What is left in the glibc hello's 41 ms: the cold scan (~10-15 ms with four workers), ELF load and VDSO mapping in the Sentry (~15 ms), stub start (~8 ms), about 25 faults and requests.

## Syscall latency per thread count
`TestSyscallLatency`: every thread has its own context in one address space, 20000 round trips each after a warm-up. (The 17-29 us of 1c came from including thread start, the first fault and the text scan of each thread in only 3000 iterations; it never was the steady state.)

| threads | per round trip, per thread (range over the runs of this series) |
|---|---|
| 1 | 0.37-0.67 us |
| 2 | 0.41-0.68 us |
| 4 | 0.54-0.98 us |
| 8 | 0.77-1.5 us |

Spinning: systrap bounds it with one dispatcher goroutine for all contexts (`fastPathDispatcher`), which stops polling after two periods of 20 us without work, and turns the fast path off when the host is oversubscribed. Winstub has no dispatcher: the waiting task goroutine spins `WINSTUB_CTLSPIN` polls (default 20000, about 10 us) and the stub spins `WINSTUB_STUBSPIN` pauses (default 3000, 20-140 us depending on the CPU) before each goes to sleep on its event, so a blocked call burns at most that much CPU on each side once. A grid of 300/1000/3000 stub pauses x 2000/20000 polls (`tuning.txt` in the artifact) showed no difference beyond the noise at 1 to 8 threads on 4 vCPUs, so the defaults stay. A bound relative to the CPU count (systrap's `controlFastPath`) matters only with many more runnable threads than vCPUs; not measured.

## Split syscalls
A `syscall` whose 0x0f ends one mapped range and whose 0x05 starts the next was not found by either scan and would have reached the NT kernel. Now each executable mapping records whether its last byte is a 0x0f that starts an instruction (the sweep reaches it) and whether its first byte is 0x05; when the two meet at the same address, the 0x05 becomes 0x0b (so the pair is `ud2`, the debugger path, resumed after it) and the later range is scanned from offset 1 (its sweep would otherwise start inside the syscall and misread the bytes after it, hiding real sites). If the high range was mapped first, its private text is rescanned from offset 1 and updated in place. A 0x0f that ends another instruction is not treated as a boundary and the next range is left alone. Test `TestSplitSyscall`: both mapping orders give the syscall (number 40, resume after the 0x05), and the "not a boundary" case executes the following `add eax,1` (rdi = 0x0f0f0f10). Limits: the 1 MiB W^X windows and the remainders a partial unmap leaves do not record edges.

## Robustness
- `TestInterruptStorm`: two contexts, one looping through a trap-table site (`inc rbx; mov eax,39; syscall`), one through a veneer site (`inc rbx; mov edi,39; mov rax,rdi; syscall`), each hammered with `Interrupt` every 30 us for 4 s: 1.3-1.5 million syscalls per context, 30-60 interrupts landed per context (the others found the thread already inside its request), 3-59 of them in veneer or trap-table code outside the guest text; after every stop rbx equals the number of syscalls seen (or one more) and every resume was correct.
- `gospin_go`: a static Go guest with four goroutines in loops without calls (only asynchronous preemption, i.e. signals, stops them) while the main goroutine forces collections for 1.5 s: passes (1.7-1.8 s) under the Sentry, covering signal delivery, full-context resume and `%fs` repairs under load.

Verdict: PASS. A free window for the Windows loader plus cached, parallel scanning cut a glibc hello from about 380 ms to 42 ms and fork + wait to about 10 ms extra, the syscall round trip stays under 1.5 us up to eight threads, syscalls split across mappings are patched in either mapping order, and an interrupt storm and a busy Go guest run clean.
