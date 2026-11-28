# 0024. The patched-text cache is relative to region bases and relocates

- Status: Accepted
- Date: 2026-11-28
- Supersedes: ADR 0022's key of the patched-text cache (history of text operations and absolute address range)

## Context
The patched-text cache of ADR 0022 was keyed by the absolute addresses of the text operations, so a PIE executable and everything in the mmap area (interpreter, libraries, vDSO), which land at random addresses in every exec, never hit it. In rewrite mode, patching libc again in every exec of a dynamic program cost about 6 ms; a loop of 100 dynamic `/bin/true` from bash took 1.5 s, against 0.43 s for static busybox (docs/results/1t-pie-cache.md).

## Decision
- The history and the key use addresses relative to the base of their region: the first text mapped below 4 GiB (executables) and above it (the mmap area). Each region moves as a whole between execs. An entry keeps the bases it was made at.
- An entry made at other bases is relocated by each region's delta. Patching records what depends on the place: guest addresses in the stub area, rel32 jumps from the text to the stub area, and the shared veneers a syscall returns into, which move to the address the platform assigns to the new site. Text with rel32 jumps is mapped as a fixed-up copy of the cached section.
- A relocation is only made when patching would make the same choices at the new place: every low-region piece is within rel32 reach of the stub area before and after, and every high-region piece is out of reach. Otherwise, and for pieces under 64 KiB, the piece is patched. Operations that do not move with a region (a piece across both regions, fixHead) put the bases into the history.
- `WINSTUB_TEXTRELOC=0` turns relocation off.

## Consequences
- An exec of a dynamic or PIE program takes the patch of its executable, interpreter and libraries from the cache. `TestTextRelocate` checks that the relocated state equals a fresh patch at the new places.
- The state of the address space carries three more lists (absAt, relAt, shared), which every patch site must keep up to date. A new kind of patch that embeds an address must record it, or relocation produces wrong code; the oracle test only covers the kinds it maps.
- Shared veneers are still allocated per site address and never reclaimed. Execs at random addresses fill that area, with or without relocation.
