# Phase 0b Windows spikes: results

Machine: Windows 11 Pro 10.0.26300, standard user, 16 vCPUs. `CPU = "QEMU Virtual CPU version 2.5+"`, CPUID.7.0:EBX = 0 (no FSGSBASE, no other leaf-7 features), TSC 2.3 GHz, `HypervisorPresent=True`. So this machine is itself a QEMU guest with a generic CPU model; timings below (especially exception and wake-up costs) may be worse than on bare metal.
Go toolchain: go1.26.2 windows/amd64 (installed), no C compiler. All spikes are pure Go plus Go assembly (`BYTE` encodings for special instructions). Nothing elevated, nothing installed system-wide.
Build/run: `cd sN; go build -o sN.exe . && ./sN.exe` (S3 and S5: see notes). Sources are in each folder.

## S0 low addresses (zpoline precondition)

Method: `VirtualAlloc` with `MEM_RESERVE|MEM_COMMIT` at fixed addresses 0x0 (treated as "any address"), 0x1000 and 0x10000, as a standard user (PowerShell P/Invoke, 2026-11-26).

| address | result |
|---|---|
| 0x1000 | fails, error 87 |
| 0x10000 | succeeds |

Verdict: FAIL. A standard user cannot map memory below 0x10000, so zpoline's trampoline at address 0 cannot be ported to Windows and syscall sites must be rewritten to traps or jumps instead.

## S1 trap cost (s1/)

Method: (a) in-process loop of `ud2` (0F 0B), caught by a vectored exception handler (`AddVectoredExceptionHandler(1, ...)`) written in Go assembly (Windows x64 ABI, no Go callback; it adds 2 to CONTEXT.Rip and returns EXCEPTION_CONTINUE_EXECUTION). Go's own handler was never involved. 7 runs x 200000 traps, timed with rdtsc (calibrated). (b) two processes (the exe re-spawned as server) sharing a 4 KB `CreateFileMapping`/`MapViewOfFile` page; client writes a request, server echoes a derived value, client waits for the answer. Spin variant: both sides busy-poll a cache-line-separated sequence counter. Event variant: auto-reset named events (`SetEvent` / `WaitForSingleObject`). 200000 (spin) and 50000 (event) round trips per run after 1000 warm-up, 3 runs.

| Test | median | p99 | notes |
|---|---|---|---|
| S1a ud2 trap + VEH resume | ~7.9-8.7 us per trap | ~8.3-9.0 us (per-run means) | 3 runs: 7.98, 7.90, 8.45 us mean |
| S1b round trip, spin | 277-298 ns | 464-521 ns | mean 325-402 ns; max outliers 0.3-13 ms (preemption) |
| S1b round trip, event | 54.2-58.3 us | 276-434 us | min 3.6-4.5 us; max up to 88 ms |

Verdict: PASS. A `ud2` caught by a raw-assembly vectored exception handler costs about 8 us per trap on this QEMU-guest machine (which bounds the slow path of a trap-based syscall design to roughly 125k traps/s per thread), while a cross-process shared-memory round trip costs about 0.29 us when both sides spin and about 55 us (p99 about 0.3-0.4 ms) when using events, so the design needs spin or hybrid waiting plus patching hot sites to avoid the exception path.

## S2 granularity (s2/)

Method: child = suspended `cmd.exe` (never resumed, then terminated). In the child: `VirtualAlloc2(MEM_RESERVE|MEM_RESERVE_PLACEHOLDER)`, split with `VirtualFreeEx(MEM_RELEASE|MEM_PRESERVE_PLACEHOLDER)`, `MapViewOfFile3(MEM_REPLACE_PLACEHOLDER)` with a pagefile-backed 1 MB section, `VirtualProtectEx`, plus content checks (`ReadProcessMemory` against the parent's own view; `WriteProcessMemory` visible in the parent view). 20 PASS lines on each of 4 runs, stable.

Results (all against the child process handle):
- Placeholder reserve returned a 64 KB-aligned base. Splitting a placeholder at any 4 KB multiple works (`split` to 4 KB, 20 KB, 64 KB all OK).
- `MapViewOfFile3` with `MEM_REPLACE_PLACEHOLDER` of a 4 KB view into a 4 KB placeholder at a 4 KB-aligned address that is not 64 KB-aligned: OK. A 64 KB view at address base+0x5000 (4 KB-aligned, not 64 KB-aligned): OK.
- Section file offset 0x1000 (4 KB) and 0x4000 (16 KB, not a multiple of 64 KB) with a 4 KB or 64 KB view: OK. Content check confirmed section offset 0x5000 appeared at view+0x1000 (`0xC0DE5000`), and a `WriteProcessMemory` into the child view was visible in the parent's mapping.
- Two adjacent 4 KB views at consecutive 4 KB-aligned addresses with different file offsets (0x10000 and 0x1000): OK, contents correct. `UnmapViewOfFile2(MEM_PRESERVE_PLACEHOLDER)` on a single 4 KB view: OK.
- `VirtualProtectEx` on a single 4 KB page inside a 64 KB view (RW to RO, and to NOACCESS): OK; `VirtualQueryEx` then reports per-4 KB regions (prot 0x4, 0x2, 0x1, then the rest 0x4).
- Without a placeholder, `MapViewOfFile3` at an explicit 4 KB-aligned address or with a 4 KB file offset fails with error 1132 (0x46C, ERROR_MAPPED_ALIGNMENT). This is the expected restriction; only the placeholder path lifts it.
- Failures that are test-order artifacts, not limits: error 487 (0x1E7, ERROR_INVALID_ADDRESS) when mapping into a range that was already replaced, or a view larger than the placeholder (needs a split first), or `VirtualAlloc2` placeholder inside an existing reservation.

Verdict: PASS. With placeholders, a non-elevated process can map section views into a child at 4 KB-aligned addresses (not 64 KB-aligned), at 4 KB-multiple file offsets, with 4 KB view sizes, and change protection per 4 KB page, so the 64 KB-window workaround in R2 is not needed on this Windows build (10.0.26300), at the cost of one placeholder split and one mapping call per 4 KB region.

## S3 TLS register / FS base (s3/) -- go/no-go

Method: `rdfsbase` (F3 48 0F AE C0) and `wrfsbase rax` (F3 48 0F AE D0) executed from Go assembly with a raw-asm vectored handler that catches the exception and skips the instruction (code reported). CPUID leaf 7 read directly. Then (only if wrfsbase had worked): FS base survival across `Sleep(1)` x200, a 3 s spin against 32 busy threads on 16 CPUs, exception dispatch (`ud2` + VEH reading FS base inside the handler), and `SuspendThread` + `GetThreadContext` + `SetThreadContext` + `Resume` from another thread (identical context, SegFs=0x53, SegFs=0). Run with `GODEBUG=asyncpreemptoff=1`. Alternatives tried: `mov fs, eax` with selectors 0x53 (CMTEB), 0x2B, 0x23, 0 (user-mode, then probe `mov rax, fs:[0]`); `NtSetInformationThread` classes 6 (ThreadDescriptorTableEntry), 29 (ThreadWow64Context), 26, 8.

Results on this machine:
- `CPUID.7.0:EBX = 0x0`, so FSGSBASE (bit 0) is not exposed by the CPU model.
- `rdfsbase` and `wrfsbase` both raise `0xC000001D` (EXCEPTION_ILLEGAL_INSTRUCTION, #UD). Everything that depends on them (survives context switch / exception / Get-SetThreadContext) could not be tested here.
- `mov fs, sel` is accepted from user mode for selectors 0x53, 0x2B, 0x23 and 0 (no exception), but afterwards `mov rax, fs:[0]` raises an access violation (0xC0000005) in every case, i.e. FS base stays 0; the selector route cannot yield an arbitrary base.
- `CONTEXT` (x64) has only 16-bit `SegFs`, no FS base field; there is no way to carry a base through `Get/SetThreadContext`.
- `NtSetInformationThread`: class 6 -> `0xC0000003` (STATUS_INVALID_INFO_CLASS), 29 -> `0xC0000004` (STATUS_INFO_LENGTH_MISMATCH), 26 and 8 -> `0xC0000003`. No documented or discovered API to set the base. (`NtSetLdtEntries` is x86-only.)
- Not testable here: whether Windows on real hardware with FSGSBASE sets CR4.FSGSBASE for user mode. This machine's virtual CPU does not report the feature at all, so the question stays open for bare metal; the test program (`s3.exe`) runs the full survival suite automatically wherever `wrfsbase` works.

Verdict: FAIL on this machine. A Linux thread's %fs base cannot be set from user mode here: `rdfsbase`/`wrfsbase` raise #UD (the virtual CPU reports no FSGSBASE), selector loads leave the base at 0, `CONTEXT` has no FS base field, and no `NtSetInformationThread` class works, so native execution of glibc code (which uses %fs for TLS and the stack canary) needs either bare-metal hardware with Windows-enabled FSGSBASE (untested) or translation/rewriting of fs-prefixed accesses.

## S4 restricted stub (s4/)

Method: parent (standard user, medium integrity) creates the child with `CreateProcessAsUser` (suspended) from `CreateRestrictedToken` derived from its own token, optional job object, and the child is the same exe in `id`/`sys N` mode. Variants: V0 normal token; V1 `DISABLE_MAX_PRIVILEGE` + deny-only Administrators + low integrity (S-1-16-4096); V2 = V1 + job (`KILL_ON_JOB_CLOSE`, ActiveProcessLimit 1, UI limits HANDLES|READ/WRITECLIPBOARD|SYSTEMPARAMETERS|DISPLAYSETTINGS|GLOBALATOMS|DESKTOP|EXITWINDOWS); V3 = restricting SIDs {RESTRICTED, Everyone} + low IL + job; V4 job only. While suspended, the parent tries `VirtualAllocEx`, `WriteProcessMemory`/`ReadProcessMemory`, `VirtualProtectEx` (4 KB), `VirtualAlloc2` placeholder + `MapViewOfFile3(MEM_REPLACE_PLACEHOLDER)`, `GetThreadContext`/`SetThreadContext`. Then the child runs and issues one raw `syscall` per disposable child with the Linux register convention (rax=nr, rdi=1, rsi=&"hi\n", rdx=3, r10=0).

Results:
- V0, V1, V2, V4: every parent operation succeeded (error 0): VirtualAllocEx, WriteProcessMemory/ReadProcessMemory, VirtualProtectEx on 4 KB, MapViewOfFile3 into the child, Get/SetThreadContext. No admin needed; `CreateProcessAsUser` with a restricted derivative of the caller's own token works without any privilege.
- V1/V2 verified from inside the child: integrity RID 0x1000 (low), 1 privilege left (baseline has 5), write to `%TEMP%` denied (Access is denied), `OpenProcess(parent, ALL_ACCESS)` denied (error 5). Baseline V0/V4 could do both. The job's UI limits and process limit were applied without error (job assignment worked; the test machine's parent is not itself in a conflicting job).
- V3 (restricting SIDs {RESTRICTED, Everyone}): token creation and all parent operations worked, but the child fails to initialize (exit `0xC0000142`, STATUS_DLL_INIT_FAILED). Restricting-SID tokens need a larger SID set or ACL-compatible locations; not pursued.
- Stray NT syscalls (Go child, raw `syscall`, Linux numbers) return an NTSTATUS in rax and do not crash, identically in V0, V1, V2 and V4: nr 0 and 9 and 231 -> `0xC0000005` (ACCESS_VIOLATION, status returned, no exception), nr 1, 12 and 60 -> `0xC0000008` (INVALID_HANDLE), nr 39 -> `0xC0000003` (INVALID_INFO_CLASS), nr 102 -> `0xC000000D` (INVALID_PARAMETER). All children exited with code 0. Linux numbers collide with live NT service numbers (the NT table is ~500 entries), so a missed `syscall` executes some NT service with the guest's register values as arguments; the restricted token only limits what it can reach.

Verdict: PASS. A standard user can start a stub with a restricted token (max privileges removed, deny-only Administrators, low integrity, optionally in a job with UI/handle and process limits) and the parent keeps full memory-mapping and thread-context control over it; a stray `syscall` with a Linux number simply runs a real NT service (observed: returns an error NTSTATUS with garbage arguments, no crash), so the restriction reduces what it could do but does not make a missed trap harmless.

## S5 Go fault recovery (s5/)

Method: NOACCESS page from `VirtualAlloc`. (a) `debug.SetPanicOnFault(true)` + `recover` for: plain Go read, Go write, builtin `copy` (runtime memmove), own assembly `REP MOVSB` read fault, assembly write fault, fault mid-copy (second page NOACCESS). (b) timing over 20000 faults; 8 goroutines x 2000 asm faults. (c) safecopy-style recovery: raw-assembly VEH registered first; when an access violation's RIP lies inside `safecopy`, it sets RIP to `safecopyFault` (returns 0) and continues; no Go panic involved. Control: same read without `SetPanicOnFault` aborts the process (`fatal error: fault`, exit non-zero).

Results (13 PASS lines):
- All six `SetPanicOnFault` cases recovered as a `runtime.Error` ("invalid memory address or nil pointer dereference", the message does not show the real address). Cost: 6.4 us per fault+recover.
- 20000/20000 repeated recoveries, and 16000/16000 across 8 concurrent goroutines (assembly faults); GC ran fine afterwards.
- Raw VEH + RIP fixup (`safecopy`): good copy returned 1; bad read, bad write and fault in the middle of a copy returned 0 (recovered); 20000/20000 repeated (5.7 us per fault), 16000/16000 concurrent.
- Without `SetPanicOnFault`, a fault in Go code is fatal.

Verdict: PASS. On windows/amd64 an access violation in Go code, in the runtime's memmove or in Go assembly (read, write, mid-copy) is recoverable via `debug.SetPanicOnFault(true)` + `recover` at about 6 us per fault, and a raw-assembly vectored handler that redirects RIP to a fixup routine (the pkg/safecopy model) works too at about 6 us per fault, both across concurrent goroutines.

## S6 WHPX (side track) (s6/)

Method: downloaded the QEMU for Windows installer (qemu-w64-setup-20260811.exe, QEMU 11.1.0) and extracted it without running it, using `7z` (NanaZip, available as an app alias; NSIS installers unpack with it) into `s6/qemu`. Alpine 3.24.4 virt ISO (sha256 verified against Alpine's published value `157d353f...b4f`); kernel `vmlinuz-virt` and `initramfs-virt` extracted from it and booted with `-kernel/-initrd -append console=ttyS0 -cdrom alpine.iso -m 512 -nographic -no-reboot`, serial console. Time measured to the `login:` prompt (`boot.sh`).

Results:
- `-accel whpx`: fails immediately: `-accel whpx: WHPX: No accelerator found, hr=00000000` followed by `failed to initialize whpx: No space left on device` (the errno text is QEMU's generic mapping; the cause is that `WHvGetCapability(WHvCapabilityCodeHypervisorPresent)` reports no usable Windows Hypervisor Platform: the optional feature is not installed/enabled, and this machine is itself a QEMU guest).
- `-accel whpx -accel tcg`: QEMU prints the error, `falling back to tcg`, and boots: login prompt after 93.3 s and 84.7 s (2 runs).
- `-accel tcg`: login prompt after 90.1 s and 85.6 s (2 runs).

Verdict: FAIL. WHPX cannot be used on this machine as a standard user (QEMU reports "WHPX: No accelerator found"), so only QEMU software emulation (TCG) works here, reaching an Alpine login prompt in about 85-93 s.

## Downloaded (all inside this folder)
- `s6/dl/qemu-w64-setup-20260811.exe` (207 MB, from qemu.weilnetz.de/w64/; not executed, extracted with 7z into `s6/qemu`)
- `s6/dl/alpine-virt-3.24.4-x86_64.iso` (70 MB, from dl-cdn.alpinelinux.org; sha256 matches)
- No C toolchain and no Go modules were downloaded.

## Re-run on a hosted runner (CI run 45847656810)

`windows-latest`, AMD EPYC 7763, 4 vCPUs, CPUID.7.0:EBX=0x219c07a9 (FSGSBASE=1).

| spike | result |
|---|---|
| S1 `ud2` + VEH | median 1747 ns per trap |
| S1 spin round trip | median 130 ns, p99 160 ns |
| S1 event round trip | median 24385 ns, p99 58744 ns |
| S2 | same placeholder results as above |
| S3 | `rdfsbase`/`wrfsbase` execute; FS base set and `fs:[0]` reads correctly; after `Sleep(1)` the next `fs:[0]` faults at address 0 |
| S4 | correction found by S7: the low-integrity restricted child dies at startup with 0xC0000142 on the hosted runner (it ran on the dev VM); normal-token children stay fully controllable |
| S5 | same results as above; 1.6 us per fault and recover |

Verdict: On a CPU with FSGSBASE, a standard-user Windows process can set its %fs base with `wrfsbase`, but Windows did not keep the value across `Sleep(1)`, so the design needs a way to restore the base after context switches; how often it is lost and whether a fault handler can restore it is measured next.

## S3 on a hosted runner (bare-metal-like CPU)

Source: `test/spikes/windows/s3`, workflow `windows-spikes` (runs 45848754957 failed on a scheduler stall that was fixed; final runs 45850082445, 45850582599, 45850991877 on `windows-latest`, AMD EPYC 9V45/7763 class, 4 vCPUs, CPUID.7.0:EBX bit 0 = 1). Ranges below are over the three clean runs; one run is quoted for the single-number lines.

Method: after `wrfsbase`, a raw-assembly vectored handler (registered first) catches an access violation whose address is below 0x10000 or non-user, compares the thread's current FS base with the expected one (per-thread slot indexed by the TEB thread id), and, if they differ, re-executes `wrfsbase` with the expected base and continues, so the faulting instruction is retried. Every repair is one observed loss. Guest-like loops read `fs:[0]`, `fs:[0x28]` and `fs:[-8]` and compare against known values.

Loss rates (base lost = it reads as 0):
- `Sleep(1)`: 2015-2021 losses in 2000 calls (about 100%); `Sleep(0)`: 0.1-0.3%; `SwitchToThread`: 0-0.3% (these rarely deschedule).
- Involuntary preemption, one guest thread, idle runner: 21-31 losses/s; with 8 non-yielding spinner threads on 4 vCPUs: 54-68 losses/s.
- Exception dispatch (2000 `ud2` + handler): 1-2 losses total, matching the background rate; the base is still set inside the handler and after resume, so dispatch itself does not clear it.
- Suspend/Get/SetThreadContext/Resume from another thread (300 rounds each, 3 ms apart): idle baseline 24-54%, Suspend+Resume 37-70%, Suspend+GetThreadContext+Resume 116-127%, plus SetThreadContext with the same context 123-143% (losses per round). Suspending a running thread deschedules it and loses the base; `CONTEXT` has no FS base field, so Set/Get cannot carry it.
- FS selector: stays 0x53 (also in `CONTEXT.SegFs`) before and at every loss; 0 selector mismatches in about 45000 losses. Only the base is lost, it goes to 0.

Self-healing:
- 4 guest threads plus 8 spinners for 10 s: 1301-1532 repairs (126-148/s), 4.4-13.1 billion loop iterations, 0 wrong values read.
- Cost per repair (`wrfsbase 0`, faulting read, handler, retry; 20000 repeats): 1.1-2.2 us for `fs:[0]` and `fs:[-8]`.
- A negative offset (`fs:[-8]` with base 0) faults at address 0xFFFFFFFFFFFFFFF8 and is repaired too; small positive offsets fault at their address (below 0x10000). Not covered: an access at an offset of 0x10000 or more from a lost (zero) base that lands on mapped user memory would silently read wrong data (Linux static TLS uses negative offsets and small positive ones, so this should not happen for glibc, but the stub must keep the low 64 KB unmapped, which Windows already enforces for a standard user).

Verdict: On a CPU with FSGSBASE, `wrfsbase` works from user mode and the base is lost to 0 at every deschedule (always across `Sleep(1)`, 20-70 times per second on a busy thread), but a raw vectored exception handler that re-executes `wrfsbase` restores it transparently: guest-like `fs:[0]`/`fs:[0x28]`/`fs:[-8]` loops ran 10 s under contention with 0 wrong reads and about 1.1-2.2 us per repair, so native %fs-based execution is feasible on FSGSBASE hardware (but not on CPUs or VMs without FSGSBASE).

## S7 exception dispatch and the guest stack (s7/)

Source: `test/spikes/windows/s7`, CI run 45856307564 (`windows-latest`, Windows Server 2025 24H2 build 10.0.26100, 4 vCPUs, normal admin runner), plus a local run on the dev VM (Windows 11 10.0.26300, QEMU guest, standard user) where the CI result differs or CI cannot run (low integrity).

Method: assembly `trap(kind, rsp)` switches RSP to a stack the spike allocated, raises one exception (`ud2`, a load from 0x10, `div` by zero, `int3`, or a store to `[rsp-8]`) and switches back. A raw-asm VEH (registered first) records the exception, the RSP it runs on, the `CONTEXT*` and `EXCEPTION_RECORD*` into a file-mapping shared with the parent, advances RIP and continues. Case 1 fills 16 KiB below RSP (and 256 B above) with a never-zero sentinel, traps, compares; run twice with complementary patterns and RSP%16 of 0 and 8. Case 2 runs each stack variant in its own child process (so a dying process is observable) and reports the exit code (the NTSTATUS) and whether the VEH ran. Case 3 uses the parent as debugger (`CreateProcess` with `DEBUG_ONLY_THIS_PROCESS`, standard user, no VEH in the child): on the first-chance `EXCEPTION_DEBUG_EVENT` it does `GetThreadContext(CONTEXT_CONTROL)`, RIP += length, `SetThreadContext`, `ContinueDebugEvent(DBG_CONTINUE)`; the child reports the sentinel result in its exit code; the low-integrity variant uses the s4 V1 token (`CreateProcessAsUser`, DISABLE_MAX_PRIVILEGE, deny Administrators, low IL). Cost is the time between consecutive exception events (QueryPerformanceCounter, 20000 traps of `ud2` in a tight loop in the child).

### 1. Red zone with a VEH on the guest stack
- Dispatch writes on the faulting thread's stack below RSP: handler SP at entry = RSP-2536 (RSP-2544 at RSP%16=8), `CONTEXT*` = RSP-1792, `EXCEPTION_RECORD*` = RSP-528 on CI (about 2.9 KB touched, farthest changed byte RSP-2912); nothing above RSP is written.
- Bytes of the 128-byte red zone overwritten: CI (Server 2025, 26100): 0 (nearest changed byte RSP-257/-265, 208-216 of the 512 bytes below RSP changed). Dev VM (Win11 26300): 88 at RSP%16=0 (nearest changed byte RSP-17), 96 at RSP%16=8 (nearest RSP-9); 232 of 512 changed, farthest RSP-2608. Same numbers for `ud2`, load, `div` and `int3`. So the clearance below RSP differs between hosts/builds (not explained here; possibly build or xsave-size dependent) and cannot be relied on: a 128-byte red zone survives on one and is clobbered on the other.

### 2. Unusable stack with an in-process VEH
Identical on CI and the dev VM, for `ud2` and for a faulting load (and a store to `[rsp-8]`):
- RSP in a committed `PAGE_NOACCESS` region, in a `MEM_RESERVE`-only region, RSP = 0, RSP = a kernel address, RSP at the start of mapped memory (nothing mapped below), and a `PAGE_GUARD` page right below 256 mapped bytes: the VEH never runs and the process terminates with the original exception as exit code (0xC000001D for `ud2`, 0xC0000005 for the load and the store). No `STATUS_STACK_OVERFLOW`, the guard page is not consumed or grown.
- RSP with only N bytes mapped below it (nothing mapped further down): N = 256, 512, 1024: dies (exit = original code); N = 2048: dies with 0xC0000005 even for `ud2` (the failed stack write itself); N = 3072, 4096, 6144, 8192: VEH runs and the process survives. The need is the dispatch frame, about 2.5-2.9 KB (`CONTEXT` + XSAVE area + record + shadow space), so at least about 3 KB of mapped stack must exist below RSP for any trapped instruction. A Linux stack-growth fault (store below the mapped stack) therefore cannot be caught by a guest-stack VEH unless 3 KB or more is already mapped below RSP.

### 3. Debugger path
- Normal token (CI): red zone intact (0 of 512 bytes below RSP changed), the child resumes after exactly one event for `ud2` and for a faulting load, also with RSP in `PAGE_NOACCESS`, reserved-only memory, RSP = 0, RSP = a kernel address, and with 0 or 256 bytes mapped below RSP: all PASS. The exception is delivered to the debugger by the kernel and never touches the guest stack.
- Low-integrity restricted token: on CI the child cannot start at all (exit 0xC0000142, same as s4 V1-V3 on the hosted runner, so it is a runner limit, not the debugger). On the dev VM (standard user) every case passes exactly like the normal token (red zone intact, unusable stacks survive, 1 event each).
- Round trip per trap (event to next event, includes `Get`/`SetThreadContext`): CI normal token median 120.8 us, p99 207 us (mean 134 us; `Get+SetThreadContext` alone median 67 us, p99 131 us). Dev VM: normal median 225 us, p99 899 us; low IL median 219 us, p99 937 us. About 70 times the in-process VEH (S1: 1.7 us per trap).

### 4. Does any exception type avoid the guest stack?
No. `ud2`, access violation, `div` by zero and `int3` all ran the handler on the faulting thread's stack at the same offset; when that stack is unusable the kernel terminates the process with the exception code instead of switching stacks (no IST-like fallback, and `PAGE_GUARD`/stack-limit logic is not applied to the dispatch frame). The only way found to avoid the guest stack is the debugger path (case 3), or never letting guest code run with an unmapped or nearly full RSP.

Verdict: A VEH on the guest stack is not safe (it overwrites a varying part of the red zone, from RSP-9 on the dev VM and from RSP-257 on the CI runner, and the process simply dies if less than about 3 KB is mapped below RSP, including on stack-growth faults), whereas a debugger parent keeps the red zone intact and survives RSP in unmapped memory (and low integrity on a standard user), at about 120-220 us per trap (roughly 70-130 times the in-process VEH), so the stub needs a debugger-based fallback (or a guarantee of 3 KB or more mapped below RSP and no red zone use) for traps on an unsafe RSP.
