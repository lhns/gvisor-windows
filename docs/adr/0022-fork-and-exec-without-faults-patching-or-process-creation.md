# 0022. Fork and exec without faults, patching or process creation

- Status: Accepted
- Date: 2026-11-27
- Supersedes: ADR 0011's one stub process per address space; refines ADR 0012 (what is cached)

## Context
A fork+exec of a static binary cost about 25 debug-event faults (0.1 to 0.4 ms each: every pma touched first, data and stack twice for copy-on-write), two copies, scans and patches of the whole text (2 ms native, 15 to 30 ms in rewrite mode on a slow CPU) and two stub process starts (5 to 10 ms each, and up to 400 ms on the development VM), against a target of 8.7 ms per iteration (docs/results/1r-platform-speed.md).

## Decision
- **Map up front** (Sentry, set by winstub): fork copies private memory of up to 8 MiB for the child and maps it and the shared translations (text) into the child's address space at once; execve maps the image (segments, vDSO, top of the stack) with the access each allows, so writable segments are copied before the first instruction; `brk` growth up to 2 MiB and `mprotect` remap at once.
- **Patched text is cached** per (history of the address space's text operations, address range), with the original bytes for an exact comparison and the whole patch state after it (stub area, tables, private text sections). The sections are shared read-only between address spaces and copied before an in-place repatch (reseam). The state is kept from the second identical patch on.
- **Stubs and their threads are recycled**: a released address space whose guest threads all wait for a reply is emptied in the background (views, patch state, checked against the committed pages of the guest range) and handed out by the next `NewAddressSpace`; its threads keep waiting and serve the next owner's contexts, whose first reply restores the whole context. A freed thread slot keeps its Windows thread the same way within an address space. A generation number keeps contexts of the old owner from touching the stub. Stubs that cannot be emptied are terminated as before.

## Consequences
- A fork child or an exec of a non-PIE binary maps memory in a few calls instead of faults, and patches nothing when the same text was patched in the same order before; PIE and dynamic executables land at random addresses and miss the text cache; they gain the rest.
- A recycled stub keeps Windows-side state of its previous owner (ntdll data, heap, the anchor's TEB). A guest that runs code outside its text can already reach the NT API (the tripwire only watches syscalls returning into guest text), so this does not weaken the boundary, but it is one more reason the boundary is the Sentry and not the stub process.
- More memory: up to 8 MiB copied per fork, text cache entries up to 160 MiB, up to 4 idle stubs and an idle Windows thread per freed slot. `WINSTUB_EAGER=0`, `WINSTUB_TEXTCACHE=0`, `WINSTUB_RECYCLE=0` and `WINSTUB_THREADREUSE=0` turn each part off.
