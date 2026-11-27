# 1p: rewrite-mode heap corruption

Verdict: there was no wrong writer: in rewrite mode `Unmap` refused every range that touched the %fs veneer area, so pages the memory manager had released stayed mapped in the stub, and a heap that shrank and grew again kept its old pages, which the Sentry no longer wrote to; `Unmap` now leaves the stub's own views alone and removes the rest, and aptrename passes in rewrite mode (RESULTS).

## Cause

- The memory manager widens an invalidation to the whole gap between its pmas (`mm/pma.go`, `invalidateLocked`: "extrapolate to the end of any previous region"), up to `layout.MaxAddr` when nothing is mapped above. A brk shrink at `0x4ec000` arrives as `Unmap(0x4ec000, 0x6fadb6d14000)`.
- In rewrite mode the 32 MiB %fs veneer area at `0x4000_0000` is a sealed stub view inside the guest range (ADR 0020). `removeRange` returns `errSealed` as soon as a range contains a sealed view, before it unmaps anything, and `Unmap` only wrote that to the debug log. So every such invalidation was dropped. Native mode has no sealed view in the guest range, which is why it passed.
- What follows: malloc trims the brk heap (`brk(0x4f4000)` down to `0x4d4000`), the stub keeps the view `0x4d3000-0x4f4000` on the old memory-file pages, the heap grows again, the memory manager hands out new pages, and the guest never faults because the old view is still there. The guest and the Sentry now see different pages at the same address: system calls write into the new ones (getdents, read), the guest reads the old ones and its own malloc metadata lives in the old ones. Ubuntu's static glibc then aborts with `free(): invalid size` at the engine's first `closedir`; Debian's (2.36) spins forever in `readdir` over a zeroed buffer after `getdents64` returned 48 bytes; apt and libsqlite3 in Podman see double frees and NULL pointers.

## How it was found

1. The development VM (no FSGSBASE, XSAVE, AVX) cannot run the CI-built guests: Ubuntu's static glibc executes `vmovd` in its start-up (`SIGILL` at `0x40446e`). Guests built on Debian bookworm run there; with them aptrename does not abort but hangs in rewrite mode (CI too: 25 minutes, then cancelled), and passes in native mode.
2. Guest probes (every DIR's pointer and chunk header recorded at opendir, checked around readdir and closedir, never changed) ruled out a writer to the chunk header and a corrupted pointer. `--strace` showed the engine's last system call: `getdents64(.../partial, 0x4db850, 0x8000) = 48`; the goroutine and stub dump showed the thread spinning at `0x439a97`, the `d_reclen` loop of `readdir`, i.e. the guest read zeros where the Sentry had just written two entries.
3. Logging every `MapFile`, `Unmap` and view change below 16 MiB: after the brk shrinks the platform got `Unmap(0x4ec000, 0x6fadb6d14000)` and friends and removed no view; the view `0x4d3000-0x4f4000` mapped at the first heap growth stayed for the rest of the run.
4. The FSFIX rollbacks of the earlier report are harmless: with the stub pid in the log, the start-up ones are the parent's `__libc_fork` errno read and the child's `%fs:0x10` after `clone`, and the errno veneer one (`neg %eax; mov %eax,%fs:(%rdx)`) is in the parent's `access()`, not in the process that dies.

Not needed in the end: hardware watchpoints (implemented, the probes made them unnecessary), the COW and patch-after-fork checks.

## Fix

- `Unmap` splits the range around the stub's sealed views and removes the rest (`aspace_windows.go`). A failing `UnmapViewOfFile2`, ignored before, is logged. `TestUnmapAcrossStubViews` (windows-platform, all legs): an unmap from a mapped page to `0x6000_0000_0000` must remove the guest pages above the veneer area and below it and keep the area.
- Found on the way: a thread slot reused by a new Windows thread kept the TEB address of the old one (`t.teb`, read once), so moving a thread out of a veneer would restore its scratch register from another thread's TEB; `createThread` now resets it (and `t.dyn`). Not the cause here (one thread per process), but Podman's Go programs reuse slots.
- `WINSTUB_FSUD2=1` was never read: the `rewrite-ud2` legs of fs-flakes ran the normal veneers, so 1o's "fails the same with every %fs instruction through the debugger" did not test that. It now sends every %fs site to the debugger path.

## Results

RESULTS-DETAIL
