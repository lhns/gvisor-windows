# 0020. CPUs without FSGSBASE: rewrite `%fs` accesses

- Status: Accepted (asked for by the owner)
- Date: 2026-11-27
- Refines: ADR 0004 (FSGSBASE condition), ADR 0010, ADR 0012

## Context
ADR 0004 required FSGSBASE because guest code addresses thread-local data through `%fs`, and the stub sets its base with `wrfsbase`. Many VMs do not expose FSGSBASE; the development machine (a QEMU guest) lacks it and also XSAVE, OSXSAVE and AVX, so `runsc.exe` refuses to start there ("the CPU lacks FSGSBASE"). The eager scan already decodes every guest instruction before it runs (ADR 0012), so `%fs`-prefixed instructions are known at patch time.

## Decision
- A second guest mode, chosen at platform start from CPUID and forceable by an environment variable: on CPUs without FSGSBASE, every `%fs`-prefixed instruction is replaced at scan time by a jump to a veneer that loads the guest's base from per-thread storage reachable through `gs:` (the stub's TEB) and runs the same instruction with a base register.
- Veneers never write to the guest stack (not even below the red zone) and never change flags; scratch registers are saved in per-thread storage.
- A fault, interrupt, signal or fork that stops a thread inside a veneer is rolled back to the site (or forward past it) before the Sentry sees the context.
- Short and rip-relative sites use the relocating-window machinery; sites that cannot be patched fall back to the debugger path.
- On CPUs without XSAVE the stub saves vector state with `fxsave`/`fxrstor`, also chosen from CPUID.
- The native FSGSBASE mode stays until Podman passes in rewrite mode; whether to keep both is decided then.

## Consequences
- runsc.exe runs on VMs and older CPUs; there is no `%fs` base to lose, so the repair path is unused in this mode.
- CI covers the mode by forcing it on the hosted runner (which has FSGSBASE); the development VM confirms it on real missing hardware.
- Each `%fs` access costs a jump and a few instructions more; the share of such sites per image is measured before building.
