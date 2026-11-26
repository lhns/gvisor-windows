# 0010. Guest faults go to the Sentry as debugger; syscalls are patched, not trapped

- Status: Accepted
- Date: 2026-11-26
- Refines: ADR 0001 (replaces the trap-first approach to catching syscalls)

## Context
S7 showed that Windows runs any exception handler on the faulting thread's own stack: dispatch writes about 2.9 KB below RSP, clobbering part of the 128-byte red zone Linux code may use (from RSP-9 on the dev VM), and the process dies if less than about 3 KB is mapped below RSP. Go reserves 4 KB of headroom on its stacks only on Windows (`stackSystem` in `runtime/stack.go`); Linux Go binaries such as Podman reserve none, so a dispatch on a goroutine stack lands in neighbouring memory. A debugging parent receives the exception with the guest's stack untouched, at any RSP, at about 120 µs per event (67 µs of it Get/SetThreadContext).

## Decision
- No exception is ever handled inside the stub on a guest stack. The Sentry attaches to each stub as its debugger and handles every guest fault (page faults, `%fs` losses, unpatched syscalls, real SIGSEGVs) from the debug event.
- Syscalls avoid exceptions: sites are patched the way gVisor's systrap `usertrap` does (`mov sysno, %eax; syscall` becomes a 7-byte absolute jump through a trap table at 0x60000) into a stub trampoline that switches to a stub-owned stack and hands the call to the Sentry over shared memory.
- The stub rewrites the `%fs` base on every return to the guest, so only preemption losses reach the debugger.

## Consequences
- The cost of a debug event becomes the cost of every page fault, `%fs` loss and unpatched syscall. It is measured without Get/SetThreadContext, together with faults per fork+exec and the share of patchable syscall sites, before the stub is built.
- If those numbers give tens of milliseconds per exec, binary translation is reconsidered; it would also remove the FSGSBASE requirement.
- The S1 and S3 numbers for an in-process handler remain valid as measurements but do not describe the production path.
