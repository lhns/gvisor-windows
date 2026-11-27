# 1s: %fs veneer capacity and faults under the jump

Verdict: the debugger path carries far under 1% of `%fs` executions in rewrite mode (python3 113 of 1,367,368, netavark 30 of 18,026, a busybox fork+exec loop 0 of 29,979), so defect 1 needed no new veneer area; a stop of a displaced instruction under the jump of a window is now reported at the window start when its predecessors can run again, and that instruction reads nothing they write; the planner keeps every other faulting instruction out from under the jump.

## Defect 1: veneer capacity, far libraries

### Measured

New counters under `GVISOR_PROFSTATS=1` (process-wide `profkeys`, so they survive recycled stubs): every `%fs` site by outcome (veneer with a 5- or 7-byte jump, or debugger and why), and every debugger run by reason. The total of `%fs` executions is the debugger count of the same workload with `WINSTUB_FSUD2=1` (every `%fs` instruction through the debugger). Job `fs-share` of `windows-profile` (Debian trixie's python3 and netavark 1.14, under `sentryguest`); it now fails at 1% or more.

| workload (rewrite mode) | debugger runs | all %fs executions | share | debugger sites, summed over the processes patched |
|--|--|--|--|--|
| `python3 py.py 1000` (json, re loop) | 113 | 1,367,368 | 0.008% | 229 |
| `python3 -c pass` | 29 | 261,886 | 0.011% | 229 |
| `netavark --version`, 3x `setup` + `teardown` (`NETAVARK_FW=none`) | 30 | 18,026 | 0.17% | |
| busybox `sh` loop, 100x fork + exec `busybox true` | 0 | 29,979 | 0% | 72 |

Final code, run 46073127366; rewrite-fxsave within a few executions of these. Before the final rollback rule (run 46067983533) the first three were 18 of 1,367,419, 18 of 261,889 and 10 of 17,596 (173 python3 sites): the rule costs sites (below) and about 100 debugger runs (10 ms) in the python3 loop.

- With the first rule the 18 debugger runs of python3 were all start-up; the final rule adds a site the loop hits about 95 times. They are "no window of 7 bytes" sites, i.e. in the high-mapped libraries (the executable is a PIE, loaded near).
- Capacity: no site fell back for "veneer area full"; the 7,895 sites of a python3 process fit in the 32 MiB area (`FSBig`) with room to spare. The ~7,000-slot limit predates that area.
- netavark is a PIE and loads at `0x1000_0000` + up to 256 MiB, within rel32 reach of the veneers: its own text is near (fsscan near-N 71, not far-N 780). Only its libraries are far.
- What was measured: Debian's netavark standalone under `sentryguest` with the platform's network stack (the first setup succeeds; later ones fail on an existing default route, after the netlink work), not the Podman image's netavark inside the engine.

### Tried and dropped: libraries near the veneers

Placing hint-less file mappings top-down in `[0x4200_0000, 0x7FFE_0000)` (an mm knob set by winstub) made every library site near: python3's debugger sites went from 173 to 68 and netavark's from 1,730 to 680, but the debugger runs stayed at 18 (python3) and went from 10 to 7 (netavark). It changes the guest's layout and cuts library ASLR to 16 bits for no measurable gain, so it is not in the branch. If a workload with hot far sites shows up, it is the change to make (commit 6d8f9c828 has it, with an ADR draft).

Also fixed: a 5-byte veneer that ended up out of rel32 reach went to the debugger; it now retries with the 7-byte jump.

## Defect 2: a fault under the jump

### Cause

A window's first instructions are replaced by the jump (5 or 7 bytes). A thread stopped at a displaced instruction is reported at that instruction's address (`fsFix`). If that address lies under the jump's bytes (not in the int3 filler, which `windowResume` handles), a signal handler that returns there with `ret` runs the jump's rel32/abs32 bytes as code. 1q kept the `%fs` instruction itself out of there and made interrupts wait; a fault of another displaced instruction there was still reported there.

### Fix (`scan.go`, `fsrewrite_windows.go`)

- `effects` records for each decoded instruction whether it can fault (`noFault`: register operands only, or lea/nop), whether it can run again with the same result (`reexec`: mov/movzx/movsx/lea/cmp/test that write only whole registers and no memory), the registers it reads and writes, and whether a stop leaves it half done (`precise`: not for string instructions, gathers, or a relocated rip-relative load, which takes two veneer instructions).
- `rollsBack(list, a, k)`: a stop at `list[k]` may be reported at the window start when `list[k]` is precise, `list[a..k-1]` are re-executable and write no register any of them reads, and `list[k]` reads none of the registers they write (destinations of read-modify-write instructions count as read; instructions with implicit register operands, such as push, div or cqo, read all). A handler that repairs a register `list[k]` uses and returns through sigreturn then resumes at the window start with its repair intact.
- `planFSWindow` accepts an instruction starting under the jump only if it rolls back, or if it cannot fault and is not the `%fs` one (then only an interrupt can stop there, and interrupts still wait, `inJump`). This replaces 1q's rule that the `%fs` instruction is first or at least one jump length in: one that rolls back may now lie under the jump.
- `unwind` reports such a stop at `w0` (the roll flag is kept per bound in the veneer), for faults and interrupts alike, so those interrupts no longer wait either.

What remains different from native: a fault in such a window is reported with the right fault address and registers, but with a pc up to 6 bytes before the faulting instruction, at instructions that run again with the same result. A handler that decodes the instruction at the pc (rather than returning, repairing or unwinding) sees a different instruction there. The alternative was a pc a handler cannot return to.

### Cost in sites (fsscan, Ubuntu binaries; debugger sites near / far)

| binary | `windows` (46060216179) | first rule (46062800012) | final rule (46071848755) |
|--|--|--|--|
| libc.so.6 | 74 / 163 | 92 / 178 | 117 / 219 |
| busybox | 26 / 59 | 31 / 64 | 42 / 83 |
| netavark | 40 / 760 | 71 / 780 | 87 / 959 |
| aardvark-dns | 56 / 232 | 50 / 214 | 81 / 283 |
| gohello | 0 / 0 | 1 / 1 | 1 / 1 |

The final rule (the faulting instruction reads nothing the earlier ones write) costs most: the common `mov %fs:..,%rax; mov (%rax),..` pair no longer rolls back.

### Tests

- `TestFSFaultUnderJump` (windows-platform, all legs): `[mov %fs:(%rdx),%r8; mov (%rbx),%rdx]` with the load faulting under the jump is reported at the window start, and a handler's `ret` there gives the right result; with `add %fs:(%rdx),%r8` first (cannot run again) no window puts the load under the jump, the fault is reported at the load, and the `ret` works too; with `mov %fs:(%rdx),%rbx` first (writes the load's base) the fault is reported at the load, the handler repairs `rbx` in the context and resumes there (sigreturn), and the load succeeds.
- `TestFSInterrupt` has a second window whose `inc` under the jump does not roll back: interrupts still never report an address under either jump.
- `TestScanFS`: unchanged in the end (its far case's `mov $8,%ecx` writes the `%fs` instruction's base, so it does not roll back).
- `TestFSRealWindows` compared fault pcs exactly; it now accepts the window start, and only for instructions under the jump of a window `FSWindows` plans. `testdata/fswindows.txt` is dumped again with the final planner (1,112 windows).

Effects are computed only for the instructions around `%fs` sites (decoded again there): computing them in the sweep for every instruction slowed every text scan.

## Interrupt retry

`interrupt` refuses a thread it cannot capture exactly (starting up, waking from its reply wait, in stub code or repair code, below the stub area, under a jump that does not roll back); the waiting Sentry thread retries after `time.Sleep(50 us)`. A yield before the sleep was tried and dropped: no A/B showed it harmless. A thread is under a jump for a few instructions, so these refusals do not explain a 212 ms interrupt.

## Checks on the final code

Product code at 770bd5234 (windows merged at bc1d28677; later commits change only windows-profile and this file):

| check | result |
|--|--|
| windows-platform, all legs (TestFSFaultUnderJump with the repair case, TestFSRealWindows on the new testdata) | green, run 46072035498 |
| windows-fsscan | run 46071848755 (table above) |
| fs-flakes, all legs | green, run 46072048650 |
| podman-windows modes=native,rewrite,rewrite-fxsave runs=4 | all 10 steps in all 12 runs, run 46072053301 |
| windows-profile fs-share (fails at 1% or more, or if a run does not exit 0) | green, run 46073127366 |
| windows-stability | 50 of 50 and 20 of 20 per leg, runs 46072070397, 46073164809 |

Earlier code (29621d134, first rollback rule, with the interrupt yield): windows-sentry-guests 46067970558, windows-runsc 46067974837, 300-run windows-stability 46067939850, all green; the final changes since are the rollback rule (planner only), the testdata and dropping the yield.

Fork+exec (`windows-stability`, median of 7, without / with 2 spare stubs, seconds). The runners differ a lot between runs ("all off" 2.2 to 3.1 s in rewrite mode), and the busybox fork+exec loop runs no `%fs` instruction through the debugger (above), so the differences are the runners':

| run | native | rewrite | rewrite-fxsave | rewrite "all off" |
|--|--|--|--|--|
| 1r final (46058080724) | 0.44 / 0.36 | 0.57 / 0.58 | 0.57 / 0.56 | 3.07 |
| 29621d134, 46067939850 | 0.34 / 0.35 | 0.45 / 0.46 | 0.67 / 0.65 | |
| 29621d134, 46068772504 | 0.35 / 0.36 | 0.46 / 0.46 | 0.55 / 0.51 | 2.17 |
| 705b2d321, 46070294722 | 0.57 / 0.46 | 0.47 / 0.47 | 0.58 / 0.57 | |
| 770bd5234, 46072070397 | 0.45 / 0.36 | 0.67 / 0.65 | 0.71 / 0.71 | 2.92 |
| 770bd5234, 46073164809 | 0.45 / 0.36 | 0.58 / 0.57 | 0.67 / 0.57 | 3.11 |

## Open

- Hot far sites in other workloads (Podman's engine, apt, sqlite) were not measured; the counters are there (`GVISOR_PROFSTATS=1`, keys `winstub %fs ...`). If one shows up, load libraries near the veneers (commit 6d8f9c828).
- The final rollback rule sends more sites to the debugger (table above); the measured share stays far under 1% (checks below). Hot sites a workload hits often could be won back by a window that starts after the instruction writing the base, where the code allows it.
