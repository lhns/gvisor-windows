# 0023. Libraries load near the veneers

- Status: Accepted
- Date: 2026-11-27
- Refines: ADR 0020 (rewrite mode)

## Context
A patched `%fs` or syscall instruction jumps to a veneer of the stub: 5 bytes (`jmp rel32`) when the veneer is within 2 GiB, else 7 (`jmp *abs32`). Linux's top-down layout puts shared libraries about 100 TiB above the veneer areas (below 2 GiB and at 1 GiB), so every library site needs a window of 7 bytes; where none exists the instruction runs through the debugger (about 100 us). Executables already load low (`PIELoadBase`).

## Decision
- File mappings without an address hint (libraries mapped by ld.so, at most 256 MiB each) are placed top-down in `[0x4200_0000, 0x7FFE_0000)`, between the `%fs` veneer area and the Windows hole below 2 GiB, the top lowered by up to 256 MiB taken from the random part of the layout (`mm.NearFileMaps`, set by winstub). When that range is full they go where Linux puts them. Anonymous mappings, the stack and hinted or fixed mappings keep the Linux layout.
- The same in native and rewrite mode.

## Consequences
- Library sites reach the veneers with 5 bytes: fewer debugger sites and shorter windows in both modes.
- Library ASLR inside the sandbox is 16 bits (256 MiB, page granular) instead of 28; the boundary is the Sentry, not the guest's layout.
- A process with more than about 1 GiB of hint-less file mappings gets the rest at the usual addresses (patched with 7-byte jumps as before).
