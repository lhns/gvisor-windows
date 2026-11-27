# 0011. Stub process design

- Status: Accepted, partly superseded by ADR 0012 (patching, fault servicing) and ADR 0022 (one stub process per address space)
- Date: 2026-11-26
- Builds on: ADR 0010

## Context
Measured: debug events cost 45-60 µs without thread context (+31 µs Get, +54 µs Set); a fork+exec takes 10-57 Sentry-handled faults; 62-82% of executed syscalls come from sites gVisor's usertrap can patch as is, and nearly all with two more patterns plus a register-number trampoline; each distinct site traps once per process (40-120 sites). One debugger thread serves 4-16 stubs at about 130k events/s. 4 KB section views into another process work with placeholders.

## Decision
- **Stub image:** a minimal PE (`stub.exe`, no CRT) started suspended. Before it runs, the controller reserves the guest range as placeholders in the stub and takes the address space; Windows' own mappings (ntdll, PEB/TEB, initial stack) stay outside the guest range, which the platform reports through `MinUserAddress`/`MaxUserAddress`.
- **Memory:** guest pages are views of the Sentry's memory-file section, mapped into the stub with `MapViewOfFile3` into split placeholders. The platform's `MapUnit` is 0 (map whole pmas per fault).
- **Threads:** one stub thread per guest thread, created by the controller (`CreateRemoteThread` in suspended state), each with a per-thread shared page (registers, syscall request, state word) and a stub-owned stack for the trampoline.
- **Syscalls:** patched sites jump through a table at 0x60000 into a trampoline that saves the guest registers to the shared page, switches to the stub stack, signals the controller (spin, then an event) and waits; on reply it restores registers, sets the `%fs` base from the shared page (`wrfsbase`) and jumps back. Patterns: usertrap's `mov $imm32,%eax; syscall`, plus `xor %eax,%eax; syscall`, `mov $imm,%rax; syscall`, and register-number sites via a 5-byte relocating jump.
- **Everything else** (page faults, unpatched sites as `ud2`, `%fs` losses, real guest faults) arrives at the controller as a debug event. `%fs` is restored by redirecting RIP to a per-thread repair trampoline (S8 method).
- **Patched code is cached:** patches are applied to the memory-file pages of file-backed text, so the next process mapping the same file page starts patched.

## Consequences
- A process start costs a few milliseconds of debug events plus the first trap of sites not yet patched in the page cache.
- The stub, trampolines and trap table are shared by the prototype (Phase 1a) and the platform (Phase 1c).
- Requires FSGSBASE (ADR 0004); tested on hosted runners.
