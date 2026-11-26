# 0012. Eager syscall patching and non-blocking debug events

- Status: Accepted
- Date: 2026-11-26
- Supersedes: the patching and fault-servicing parts of ADR 0011

## Context
ADR 0011 copied two assumptions from gVisor's Linux systrap platform that do not hold on Windows. First, systrap patches a syscall site after its first trap, because seccomp delivers every syscall; on Windows an unrewritten `syscall` runs an NT service and the debugger never sees it (S4). Second, a debug event suspends every thread of the debuggee until it is continued, so servicing a blocking syscall inside the event (a futex wait in a Go guest, `wait4`, `read`) would freeze the threads that could wake it.

## Decision
- **Eager patching:** before a guest page becomes executable, it is scanned completely and every syscall site gets its final patch (a jump for patchable sites, `ud2` otherwise). No guest code ever runs unscanned. Writes to an executable page drop its execute permission; anonymous executable memory (JIT) is scanned on `mmap`/`mprotect` with `PROT_EXEC`.
- **Patches are private:** patches go to the address space's private copy of a text page, never to the memory-file pages that back the file, so `read()` and directfs write-back see the original bytes. A per-file cache holds the list of sites, not patched pages.
- **Debug events are short:** the handler only fixes the cause (maps a page, restores `%fs`, or moves a `ud2`-site syscall into the trampoline entry) and continues at once. All syscalls are serviced over shared memory while the debuggee runs.
- **Correctness of the scan:** the scanner's instruction boundaries decide correctness; byte pairs `0f 05` that the decoder does not read as instructions are counted and reported per binary.

## Consequences
- The "first trap per site" cost of ADR 0011 disappears; a process start costs its page faults plus the scan.
- Interrupting a running guest thread (signals, preemption) needs thread suspension from the controller; its cost is measured in the prototype.
- ADR 0011's process layout, memory and `%fs` decisions stay in force.
