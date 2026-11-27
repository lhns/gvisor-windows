# 1q: a thread that returns into a patched %fs window

Verdict: a thread that a signal handler returns into a patched window with `ret` now continues in the veneer (int3 filler, `windowResume`), and interrupts wait only while a thread would be reported under the jump; Podman passes all 10 steps 16 of 16 in rewrite and rewrite-fxsave, and the `TestFSInterrupt` "bit 63" failure was the test's own overflow, caused by a blanket interrupt refusal that is gone.

## Cause

A Podman engine (rewrite mode) died with a fault at the nop filler of a patched window plus rax. The platform reports a thread stopped inside a veneer at the guest instruction it belongs to (`fsFix`), which can be an inner instruction start of the window. The Sentry translates such an address back into the veneer when it resumes the thread (`restartAt`). But a signal handler that returns there itself, with a plain `ret` as Go's `asyncPreempt` does, ran the bytes that replaced the window: the jump's tail or the filler.

## Fix

- **int3 filler, `windowResume`.** The filler after the jump is int3. A thread that traps at a registered inner instruction start is sent into the veneer there (`fsrewrite_windows.go`, from the breakpoint case in `process_windows.go`). `TestFSReturnIntoWindow` returns into a window with `ret` after a fault of its `%fs` instruction.
- **The `%fs` instruction never lies under the jump.** A fault of its access is reported at its address, so `planFSWindow` keeps only windows where it is first or starts at least one jump length in. A first try that required every window to begin with an instruction as long as the jump sent about 200 more libc sites (600 more netavark ones) to the debugger. The rule costs far windows (7-byte jump) whose `%fs` instruction follows a 5- or 6-byte one: `TestScanFS` now expects one window less far than near.
- **Interrupts.** An interrupt that stops a thread in a veneer can report any inner address: a displaced instruction or the end of the `%fs` instruction, which may lie under the jump. The first version refused every thread inside a veneer and retried. That starved a loop of `%fs` accesses: `TestFSInterrupt` took 8.4 s instead of 0.3 s for its interrupts (about 100 ms each, against under 1 ms before). Its counter passed 2^32, and the expected value, `rcx*(rcx-1)/2` in 64 bits, wrapped and lost bit 63. That was the "bit 63 wrong" failure; the guest's counter was right. Now `interrupt` refuses only a thread that would be reported strictly inside the jump's bytes (`fsVenInfo.inJump`). Threads in the thread's own veneer of a ud2 site (`t.dyn`) are never refused.
- **Tests.** `TestFSInterrupt` has a window whose `%fs` instruction comes first, so a displaced `inc` lies under the jump. It checks that the planner builds that window (`FSWindows`) and that no interrupt reports an address under the jump, computes the counter without overflow, and fails if 400 interrupts take more than 10 s.
- **fs-flakes.** The rewrite-ud2 leg sends every `%fs` instruction through the debugger. Its fsstress took about 11 min per phase at 600 iterations (run 46016193396), then hit the 15 min deadline 3 of 3 on the blanket refusal. It now runs a quarter of the iterations. Whether the blanket refusal made it slower is not measured: on bce41cf (no refusal) a phase took about 11 min at 600 iterations, on this branch's first version more than 15 min, on the final code about 65 s at 150 (about 2.5 times faster per iteration than bce41cf). The refusal is the suspect.

## Results

- windows-platform, full suite, native, rewrite and rewrite-fxsave: green (run 46037345150). `TestFSInterrupt` 0.27 / 0.38 s.
- fs-flakes, all legs: green (run 46036042807). rewrite-ud2 fsstress at 150 iterations: 2m10 to 2m19 per repetition, 3 of 3.
- podman-windows `modes=rewrite,rewrite-fxsave runs=8` on 6ba947a86: all 10 steps 16 of 16 (run 46037364923; before the interrupt change, run 46026228865 also 16 of 16).

## Open

- A fault of a displaced instruction under the jump is still reported there. A handler that returns there with `ret` would run the jump's bytes. No case is known.
- `runsc.exe` is tracked at the repository root on `windows`. A local `go build ./runsc` overwrites it.
