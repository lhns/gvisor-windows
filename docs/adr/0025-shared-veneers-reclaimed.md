# 0025. Shared veneers are reclaimed when no address space holds them

- Status: Accepted
- Date: 2026-11-28
- Refines: ADR 0022 and ADR 0024 (the shared veneers they leave unreclaimed)

## Context
A syscall site whose patch displaces instructions after the syscall returns into its veneer, and a fork child resumes there, so the veneer of a site (address and bytes) is at the same address in every address space, handed out by a platform-wide registry. The registry never gave space back: every exec of a dynamic program at a new address took more of the 224 KiB area, and after about 500 execs in one sandbox it was full; relocations were refused and the sites went to the debugger (docs/results/1t-pie-cache.md). A Podman engine keeps one sandbox for days.

## Decision
- Holders: an address space holds every shared veneer it got until it is released or emptied, unmaps included. A thread stopped in a syscall or a signal frame may still return into the veneer of an unmapped site, and the platform sees neither, so unmap gives nothing back. A cached patch state holds the veneers it lists.
- A veneer without holders keeps its address. When the area has no room left, idle veneers are given up, the longest idle first, and their space is reused (first fit, merged ranges).
- A snapshot of the patch state copies only its own shared veneers, not the whole area, so applying it never writes over a veneer the address space holds for an unmapped site, or stale bytes of an earlier owner of a recycled stub.
- Keying veneers relative to region bases (so relocated pieces reuse them) was not done: it would make identical bases in a fork parent and child a correctness condition.

## Consequences
- The area holds the veneers of live address spaces and cached states; execs at random addresses no longer fill it.
- A fork child that maps its text only after its parent was released, at a time the area is full, may find its veneer reused. Fork maps the child's text up front (ADR 0022), so this needs `WINSTUB_EAGER=0` or a failed eager map as well.
- A process that maps and unmaps many libraries at new addresses keeps their veneers until it exits.
