# 1s: %fs veneer capacity and faults under the jump

Verdict: the debugger path already carries far under 1% of `%fs` executions in rewrite mode (python3 18 of 1,367,377, netavark 7 to 10 of about 17,000), so defect 1 needed no new veneer area; a stop of a displaced instruction under the jump of a window is now reported at the window start when its predecessors can run again, and the planner keeps every other faulting instruction out from under the jump.

## Defect 1: veneer capacity, far libraries

### Measured

New counters under `GVISOR_PROFSTATS=1` (process-wide `profkeys`, so they survive recycled stubs): every `%fs` site by outcome (veneer with a 5- or 7-byte jump, or debugger and why), and every debugger run by reason. The total of `%fs` executions is the debugger count of the same workload with `WINSTUB_FSUD2=1` (every `%fs` instruction through the debugger). Job `fs-share` of `windows-profile` (Debian trixie with python3 3.13 and netavark 1.14, under `sentryguest`); it now fails at 1% or more.

| workload (rewrite mode) | debugger runs | all %fs executions | share | debugger sites |
|--|--|--|--|--|
| `python3 py.py 1000` (json, re loop) | 18 | 1,367,377 | 0.0013% | 173 (68 with libraries near) |
| `python3 -c pass` | 18 | 261,890 | 0.007% | 173 |
| `netavark --version`, 3x `setup` + `teardown` (`NETAVARK_FW=none`) | 10 | 16,726 | 0.06% | 1,730 (680 near) |

Run 46063676341 (this branch's planner, with and without the library placement below); the final code gives the same counts in both rewrite legs (run 46067983533: python3 18 of 1,367,419, start-up 18 of 261,889, netavark 10 of 17,596 in rewrite; rewrite-fxsave the same within a few executions). The baseline is this branch with the defect-2 planner, not unmodified `windows`; the planner changes the far debugger sites of libc by 15 (below), which start-up touches at most once each.

- The 18 debugger runs of python3 are all start-up: the loop adds none. They are "no window of 7 bytes" sites in libc, mapped high.
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
- `rollsBack(list, a, k)`: a stop at `list[k]` may be reported at the window start when `list[k]` is precise and `list[a..k-1]` are re-executable and write no register any of them reads.
- `planFSWindow` accepts an instruction starting under the jump only if it rolls back, or if it cannot fault and is not the `%fs` one (then only an interrupt can stop there, and interrupts still wait, `inJump`). This replaces 1q's rule that the `%fs` instruction is first or at least one jump length in: one that rolls back may now lie under the jump.
- `unwind` reports such a stop at `w0` (the roll flag is kept per bound in the veneer), for faults and interrupts alike, so those interrupts no longer wait either.

Semantic change: a fault in such a window is reported with the right fault address and registers, but with a pc up to 6 bytes before the faulting instruction, at an instruction that runs again. A handler that decodes the instruction at the pc (rather than returning, retrying or unwinding) sees a different instruction there. Faults of TLS accesses and of the few instructions displaced with them are rare; the alternative was a pc a handler cannot return to.

### Cost in sites (fsscan, Ubuntu binaries; debugger sites near / far)

| binary | before (46060216179) | after (46062800012) |
|--|--|--|
| libc.so.6 | 74 / 163 | 92 / 178 |
| busybox | 26 / 59 | 31 / 64 |
| netavark | 40 / 760 | 71 / 780 |
| aardvark-dns | 56 / 232 | 50 / 214 |
| gohello | 0 / 0 | 1 / 1 |

### Tests

- `TestFSFaultUnderJump` (windows-platform, all legs): `[mov %fs:(%rdx),%r8; mov (%rbx),%rdx]` with the load faulting under the jump is reported at the window start, and a handler's `ret` there gives the right result; with `add %fs:(%rdx),%r8` first (cannot run again) no window puts the load under the jump, the fault is reported at the load, and the `ret` works too.
- `TestFSInterrupt` has a second window whose `inc` under the jump does not roll back: interrupts still never report an address under either jump.
- `TestScanFS`: the far jump now gets the window whose `%fs` instruction starts 5 bytes in (it rolls back).
- `TestFSRealWindows` compared fault pcs exactly; it now accepts the window start, and only for instructions under the jump of a window `FSWindows` plans.

Effects are computed only for the instructions around `%fs` sites (decoded again there): computing them in the sweep for every instruction slowed every text scan.

## Interrupt retry

`interrupt` refuses a thread it cannot capture exactly (starting up, waking from its reply wait, in stub code, under a jump that does not roll back); the waiting Sentry thread retried after `time.Sleep(50 us)`, which lasts 0.5 ms or more on Windows. It now yields (`runtime.Gosched`) for the first 64 retries.

## Checks on the final code

On 29621d134 (windows merged in at bc1d28677):

| check | result |
|--|--|
| windows-platform, all legs (with TestFSFaultUnderJump) | green, run 46067943986 |
| windows-sentry-guests, all legs | green, run 46067970558 |
| windows-runsc, all legs | green, run 46067974837 |
| fs-flakes, all legs | FILLFF |
| podman-windows modes=native,rewrite,rewrite-fxsave runs=4 | all 10 steps in all 12 runs, run 46067979033 |
| windows-profile fs-share (fails at 1% or more) | green, at most 0.057%, run 46067983533 |
| windows-stability, 300 runs per leg | 300 of 300 in every leg, run 46067939850 |

Fork+exec (`windows-stability`, median of 7, without / with 2 spare stubs, seconds); the runners differ by up to 50% between runs (rewrite-fxsave "all off" 2.2 to 2.9 s), so three runs:

| run | native | rewrite | rewrite-fxsave |
|--|--|--|--|
| 1r final (46058080724) | 0.44 / 0.36 | 0.57 / 0.58 | 0.57 / 0.56 |
| 46067939850 | 0.34 / 0.35 | 0.45 / 0.46 | 0.67 / 0.65 |
| 46068772504 | 0.35 / 0.36 | 0.46 / 0.46 | 0.55 / 0.51 |
| ab083bfb0 (effects in the sweep), 46065397940 | 0.56 / 0.46 | 0.40 / 0.36 | 0.46 / 0.46 |

## Open

- Hot far sites in other workloads (Podman's engine, apt, sqlite) were not measured; the counters are there (`GVISOR_PROFSTATS=1`, keys `winstub %fs ...`). If one shows up, load libraries near the veneers (commit 6d8f9c828).
- `testdata/fswindows.txt` (TestFSRealWindows) was dumped with the old planner; it still covers the real windows, but not those the new rule plans.
