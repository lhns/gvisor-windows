# 0011. The memory file is one SEC_RESERVE section per 1 GB chunk

- Status: Accepted
- Date: 2026-11-26

## Context
pgalloc's MemoryFile is a memfd mapped in 1 GB chunks. On Windows its pages must live in a section so that later mappings into stub processes (S2: placeholders and `MapViewOfFile3`, 4 KB granularity) see the same memory. Views of a section cannot be decommitted, and `SEC_COMMIT` charges a section's full size to the commit limit at creation.

## Decision
- Each chunk is its own pagefile-backed section, created `SEC_RESERVE` and mapped once into the Sentry. Pages are committed with `VirtualAlloc(MEM_COMMIT)` when `Allocate` hands them out.
- Decommit zeroes with `DiscardVirtualMemory` (checked, with a clear as fallback); pages stay committed.
- `MemoryFile.Sections(fr)` exposes the section handle and offsets per chunk for the platform.

## Consequences
- Commit charge follows the high-water mark of allocations; freed pages keep their charge. A real release needs smaller sections that are closed when empty, which changes `chunkInfo` and the platform's mapping code; decide it with measurements from real stubs.
- Several handles instead of one, so a consumer maps per chunk.
- No lazily faulted memory: every allocation commits, so first-touch cost moves to `Allocate`.
