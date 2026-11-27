# 1m: platform repair (native %fs repair, seams, tripwires, spare stubs)

Verdict: the native-mode Go GC crash is reproduced outside Podman (a Go guest with tight loops around an assembly call plus a page-fault storm dies in 12 of 12 runs with the old 20-in-5-ms give-up rule, 0 of 12 with the repair that decides from the fault address), the Debian `ld.so` "file too short" was not a seam decode problem but a call from another piece of text landing inside a syscall patch window (fixed, the `debian` step is now required), a process instrumentation callback works as a tripwire for unpatched syscalls (also for a standard user), the 3-second thread replacement is off, and spare stub processes replace stub recycling (numbers in section 5).

## 1. Native-mode GC crash

Hypothesis of the brief confirmed in part: the faulting load is `mov %fs:-8,%r14` (reload of g after an ABI wrapper call) with a zero base; the repair gave up. Two corrections:

- The rdfsbase-compare trampoline cannot work. The event that sends the thread to the routine has dropped the base already (`rdfsbase` is 0 at every entry, also for a genuine fault), so the compare says "lost" every time: `signals segv` hung with 1.1 million events. Dropped in favour of deciding in the debugger.
- The decision is made from the fault itself (`fsBaseLost`): the instruction at `rip` is taken apart (`parseFS`), its operand without a base is `disp + base reg + index reg << scale`; a fault at exactly that address is a lost base (repair), a fault at that plus the thread's `%fs` base is the guest's own (reported), an instruction without `%fs` prefix is never repaired (a null dereference used to be repaired thousands of times before it was reported). An undecidable instruction is repaired up to 100000 times in a row. No count or time heuristic for decidable ones: a tight loop may lose its base at the same instruction any number of times.

Reproduction: `test/guests/gogc` (12 threads allocating, 4 threads touching fresh 4 MiB mappings, 4 tight loops calling an assembly `nop()` so that the ABI wrapper reloads g from `%fs:-8` at one instruction), run by `windows-gcrepro` (native mode, 20 s, 12 runs per variant, three platform versions built from git):

| variant | runs ok |
|---|---|
| old: repair gave up after 20 repairs in 5 ms (before 73227a2f1) | 0 of 12 (the guest dies in under a second with a Go crash) |
| base: 2000 in 20 ms (the state at the start of this work; effectively never gives up) | 12 of 12 |
| fix: decision from the fault address | 12 of 12 (about 3200 repairs and 16000 debug events per run, none reported) |

So the old rule is the crash; the 2000/20 ms rule of 1l already hid it, and the Podman crash was last seen before that. The page-fault storm gives about 1300 events per second; the repair counters are in the state dump (`%fs repairs N (reported as faults M)`).

## 2. Syscall seam rule, tripwires, the debian case

- `reseam`: text pieces arrive in any order. A piece mapped with nothing below keeps its first 576 original bytes (`heads`); when the piece below arrives, the seam is decoded again with the real bytes below (the last bytes of the lower piece from a real instruction boundary, `tailB` of the scan, else 128 bytes), the guessed boundaries are compared with the real ones, every patch that touches the part before they agree is put back to the original bytes and that part is patched again (also `%fs` sites, the same `patchCode`). `TestSeamRedecode` (both orders; a syscall swallowed by a guessed instruction, a `0f 05` inside an immediate) passes; `fixHead` stays for text that an unmap cut. The fixed 48-byte zone remains only for a piece with nothing below it.
- Tripwire (`WINSTUB_TRIPWIRE=1`, on in the platform, sentry-guests and runsctest runs): `NtSetInformationProcess(ProcessInstrumentationCallback, 40)` on the stub's own process, called by the anchor thread from the stub (`icbInit`); the callback (`icb`) keeps every register and flag, runs on a small stack in the thread's page, and ends the address space with a loud `TRIPWIRE` log line when the kernel returns into guest text right after a `0f 05`. Works for the admin and for a standard user (`windows-icb`: status 0, about 100 hits for 300 calls). `TestTripwire` (patched site put back to its original bytes) passes. It found one real hole at once: text cut by an unmap (`TestSplitSyscallAfterUnmap`, fixed by keeping `fixHead`).
- `%fs` tripwire (rewrite mode, the coordinator's request): in rewrite mode the base is always zero, so a fault of an `%fs`-prefixed instruction is a site the patcher missed; it is logged with its address and bytes (`FS TRIPWIRE`) before the SIGSEGV is delivered. Not yet seen in a run; the engine's `addr=0x28` death of run 45962927170 would have been reported with its site.
- The debian case (`runsctest -only debian`, `/dbin`, root folder): the text arrives in two pieces, and the seam is not involved (reseam changed 6 bytes that needed no patch). Reproduced locally on the development VM with the CI's `dbin` files and WINSTUB_DEBUG. `__read_nocancel` is `xor %eax,%eax; syscall` after 10 bytes of nop padding; the planner took the padding, the `xor` and the `syscall` as one 14-byte window (`jmp rel32` plus filler), and `open_verify`, in the other piece, calls the function at the `xor`, which is inside the window: the call ran the filler and fell out behind the syscall with an undefined `rax`, so ld.so saw "file too short" and no `read` ever reached the Sentry (and the tripwire stayed silent: no syscall ran). Branches from other pieces are invisible to the scan; fix: the instruction after padding (`nop`), `ret` or an unconditional `jmp` counts as a branch target, so no window extends across it. `TestEntryAfterPadding` (call from another piece, both orders). The `debian` step of `windows-runsc` is now required.

## 3. The 3-second thread replacement

Off by default (`WINSTUB_REPLACE_STALLED=1` brings it back): a thread that has not run its entry code after 15 s is an error (the task fails, `replaced threads` in the dump counts it), instead of terminating it inside ntdll's start-up and creating another (which can leave the loader lock held). Podman x8 comparison (`podman-windows`, modes `native` and `native-replace`, 8 runs each): see section 6.

## 4. Not done

- Recycling a stub across exec (reset of views, tables and slots); replaced by spare stubs (below).
- Islands near high-mapped libraries; a jump table entry or a call from another piece into the middle of a window that does not follow padding, `ret` or `jmp` is still invisible (the tripwire finds the syscall variant only if a syscall runs natively, not when it is skipped).
- A `%fs` test for the seam rule (the same code path, `TestSeamRedecode` covers syscalls only).

## 5. Spare stub processes

`WINSTUB_SPARES` (default 2): the platform starts stubs in the background and `NewAddressSpace` takes a started one, so fork and exec do not wait for process creation. `windows-stability` times the 100x fork+exec loop with spares off and on (`FORKEXEC` lines): see section 6.

## 6. Results

Commit under test: see the head of branch `platform-fs-repair`.

- windows-stability (300 fork+exec runs, 16 s limit): native 300 of 300, rewrite 300 of 300, rewrite-fxsave 300 of 300 (run 45968501662).
- Fork+exec of 100 `/bin/true` (median of 7, same job, spares off / 2): native 1.96 / 1.95 s, rewrite 3.75 / 3.16 s, rewrite-fxsave 2.49 / 2.45 s. Spares gave no gain except in the noisy rewrite leg, so process creation is not what dominates (about 20 ms per iteration native: the Sentry's fork, exec, the pma mapping and the patching of the binary are); the 4x target is not met. Default is now `WINSTUB_SPARES=0`; the code stays for a later measurement. Real recycling would save the same process creation and is not expected to reach 4x either; profiling the 20 ms (windows-profile) is the next step.
- Podman acceptance x8 per mode (run 45968529454, native / native with the 3-second rule on): no engine crash and no hang in any run of either; steps 1-4, 6-8 pass everywhere, step 5 (apt) fails as known; step 9 and 10 failed in run 3 of `native` and in runs 3 and 5 of `native-replace` (7 of the 8 runs of that mode produced logs). So the rule off is not worse than on.
- windows-platform, windows-sentry-guests (all legs), windows-runsc (all legs, `debian` required and passing), windows-gcrepro, windows-fs/net/sentry/rootfs/fsscan: green on the commits listed in the final report. One `tty` timeout ("sleep not interrupted by ^C") in the rewrite leg of one windows-runsc run, not seen again.
