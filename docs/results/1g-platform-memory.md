# Phase 1g: files without sections, memory release, edge cases, oversubscription

Nodes: defect "Map files not in the memory file" (platform-map-files-without-sections), defect "Give freed guest memory back" (platform-memory-release), and the open limits of 1e (W^X windows and remainders for split syscalls, spin bounding). Everything ran on `windows-latest` (4 vCPUs); nothing was run on the dev machine.

CI: platform tests https://github.com/lhns/gvisor-windows/actions/runs/45897022077 (`windows-platform.yml`); `runsc run` with the erofs root, https://github.com/lhns/gvisor-windows/actions/runs/45897244424 (`windows-runsc.yml`, dispatched on this branch); Sentry guests green on the same head (run 45897022059). `windows-fs` failed once on this head in `TestVFSNegativeCacheAndQuickChange` (a timing test of the gofer backend, passed on the previous head; not touched by this work).

## Files that are not the memory file
`runsc run` with an erofs image as root died on the first instruction fetch: erofs maps file pages from its image mapping (`imageMemmapFile`), which is not a `pgalloc.MemoryFile`, and `MapFile` insisted on sections. Now `MapFile` takes any `memmap.File`:
- text (read and execute) was already copied out with `MapInternal`, scanned and patched into a private section; it no longer needs the file to expose sections;
- any other read-only mapping of a file without sections is copied into a private section (`PAGE_READONLY` view); the cost is a copy of the mapped range per mapping, and no sharing between processes (an erofs binary mapped by ten processes is ten copies);
- a writable mapping of such a file is refused (`errNoSection`): a copy would not be shared with the file.
`windows-runsc.yml`'s "Erofs image root as a standard user" step is required again and passes, as does the rest of the suite (busybox, stdin, net, publish, tty). The earlier attempt with `useReadForIO` ended in `function not implemented` in the loader's read path; it was not chased, since the image mapping works now. While running busybox from the image a decoder panic showed up (`x86asm.decodeAVX` on a truncated AVX instruction at the end of a range): the scan now survives a panic of the decoder (it skips that byte). Test `TestMapNonSectionFile`.

## Giving freed memory back (ADR 0016)
Design: a chunk is one placeholder in the Sentry; it is filled with one pagefile-backed section per 2 MiB extent in use, created on allocation and unmapped and closed when a decommit covers the extent and its pages are free (the releaser's path); `Decommit` of pages that stay allocated discards but keeps the section; `Sections` returns one range per extent. See `docs/adr/0016-memory-file-on-extent-sections.md` for why smaller sections (and not `VirtualFree` on views, `MEM_RESET` or `OfferVirtualMemory`, which keep the commit charge) and what it costs.

Measured (`TestMemoryRelease`: allocate 1 GB, touch every page through `MapInternal`, map the 1 GB into a stub, unmap it, `DecRef`; system commit charge and this process's working set):

| | commit charge | working set |
|---|---|---|
| baseline | 2728-2830 MB | 9 MB |
| 1 GB allocated, touched, mapped in a stub (0.7-0.9 s) | +1030 MB | +1024 MB |
| 0.15-0.2 s after the free (512 extents released) | +0-32 MB | +0-29 MB |

Before (1b, `sentrysmoke`, ADR 0013): the working set dropped by about half of the touched pages with one view, not at all with a second view mapped, and the commit charge never went down. `tools/windows/sentrysmoke` adapts to per-extent sections. Cost: memory comes back in 2 MiB units (a freed range smaller than an extent only loses its physical pages), 512 handles and 512 views per GB in use.

## Edge cases of split syscalls (`TestSplitSyscallAfterUnmap`, `TestSplitSyscallWX`)
The edge bookkeeping now covers the cases 1e listed as limits: when a partial unmap cuts a private text view, the surviving ends are examined before the unmap (the left piece's last byte is a 0x0f that starts an instruction, found with the scan; the right piece starts with 0x05) and recorded; the 1 MiB W^X windows go through the same routine as ordinary text (`patchText`). Tests: a left remainder (0f 1f 00 cut at a page boundary, then new text starting with 05 mapped there), a right remainder (a page that starts with 05, its left neighbour removed and a new one ending in 0f mapped), and a syscall split between two W^X windows of a read-write-execute mapping: each gives the syscall (via ud2) and then the real one after it.

## Spin bounding with many more runnable threads than vCPUs (`TestOversubscribed`)
32 busy guest threads (8 per vCPU) in a syscall loop for 4 s, plus one probe thread timed round trip by round trip (QPC):

| spin policy | syscalls per second | per thread min / mean / max | probe p50 / p99 / p99.9 / max |
|---|---|---|---|
| always spin (as before) | 1.5-2.0 million | 14k-62k / 188k-246k / 367k-620k (up to 40x apart) | 1.2 us / 5.8 us / 69 us / 1.1 s in one of three runs |
| spin only while at most as many contexts run as there are vCPUs (now) | 0.25-0.29 million | 25k-27k / 32k-37k / 39k-52k (within 2x) | 10-11 us / 104-124 us / 22-35 ms / 92 ms |

Always spinning is the faster in total, but it starves threads (up to 40x difference in progress) and produced a one-second stall for the probe. Now `Switch` counts the contexts inside it and, above the number of CPUs, neither the waiting Sentry goroutine nor the stub spins before sleeping on its event (limit `WINSTUB_SPINLIMIT`, default the CPU count). A bug found on the way: the stub counts its spin budget down to zero, so a budget of 0 is 2^64 iterations; "no spinning" is a budget of 1. Latency up to 4 busy threads is unchanged (0.4-1.6 us); at 8 threads on 4 vCPUs the round trip is 20-28 us without spinning (1.6 us with `WINSTUB_SPINLIMIT=1000`), the price of fairness there.

Verdict: PASS. Any `memmap.File` can now be mapped (erofs images boot under `runsc run`), a freed gigabyte goes back to Windows in 0.2 s (working set and commit charge, with the stub's views mapped and unmapped), split syscalls are covered at cut and window edges, and 32 busy threads on 4 vCPUs share the CPUs within a factor of two instead of one thread getting forty times the others.
