# 0016. The memory file is made of 2 MiB extent sections, closed when freed

- Status: Accepted
- Date: 2026-11-26
- Supersedes: ADR 0013 (one SEC_RESERVE section per 1 GB chunk)

## Context
ADR 0013 backed each 1 GB chunk of pgalloc's MemoryFile with one section created `SEC_RESERVE` and committed pages with `VirtualAlloc(MEM_COMMIT)`. Windows cannot decommit part of a section through a view, so freed memory was only discarded (`DiscardVirtualMemory`): the physical pages left, but they stayed committed (system commit charge never went down) and, with a second view mapped (a stub process), they stayed in that view's working set.

## Decision
- A chunk is an address range of the Sentry reserved as one placeholder (`VirtualAlloc2`, `MEM_RESERVE_PLACEHOLDER`), so the memory file stays one contiguous mapping for `MapInternal`.
- The range is filled in 2 MiB extents. An extent in use is its own pagefile-backed section (committed when created), mapped into the placeholder with `MapViewOfFile3(MEM_REPLACE_PLACEHOLDER)`. It is created when its pages are allocated (`Allocate`, or on first access through the chunk mapping) and read as zero.
- When a decommit covers an extent entirely and the pages are free (`decommitFile`, called by the releaser for freed ranges), the view is unmapped back to a placeholder and the section handle is closed. Working set and commit charge fall once no other view of the section is mapped; stub processes unmap their views before the pages are freed, as the memory manager does for every address space.
- `Decommit` (the pages stay allocated and may be mapped) keeps the section and discards the pages instead (`decommitRetained`).
- `Sections(fr)` returns one range per extent; the platform maps one view per extent.

## Consequences
- 1 GB allocated, touched, mapped into a stub and freed returns to Windows in about 0.2 s: working set and commit charge are back at the baseline (docs/results/1g-platform-memory.md).
- Memory is returned in 2 MiB units: a freed hole smaller than an extent only drops its physical pages. Fragmentation can keep up to 2 MiB committed per live allocation.
- More kernel objects: 512 section handles per GB in use, and the platform maps 512 views per GB (about 4 ms per GB).
- A section handle returned by `Sections` is valid only while its pages are in use.
