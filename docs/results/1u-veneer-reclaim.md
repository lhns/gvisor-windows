# 1u: shared veneers reclaimed, host I/O out of Sentry locks

Verdict: shared veneers are now given back when the last address space or cached state holding them goes, so 1000 execs of a dynamic `/bin/true` in one sandbox relocate every piece (1996 of 1996, 0 refused, 0 "area full") at a constant 0.7 ms per relocation, where before 980 relocations were refused after about 500 execs; and the three host I/O calls named in 1t no longer run under mount-wide or address-space locks.

Nodes: `platform-shared-veneers-fill`, `sentry-host-io-under-locks`. Design: ADR 0025.

## 1. Shared veneers

### Cause

A syscall site whose patch displaces instructions after the syscall returns into its veneer, and a fork child resumes there, so `sharedVeneer` handed out one address per (site address, original bytes) for the whole platform and never took it back (ADR 0022, 0024). Each exec of a dynamic program puts libc at a new address, so each took about 7 more veneers of the 224 KiB area; around exec 500 it was full (1t, run 46101653867).

### Change (`veneers_windows.go`, ADR 0025)

- The registry counts holders per veneer. An address space holds every veneer it got (`as.vheld`) until `Release` (synchronously, before the stub is emptied in the background), whether the site is still mapped or not: a thread stopped in a syscall, or a signal frame, may return into the veneer of an unmapped site, and the platform cannot see either, so unmap gives nothing back. A cached patch state (`patchSnap`) holds the veneers it lists, from `snapshot` to `release`.
- A veneer without holders keeps its address; only when the area has no room left are idle veneers dropped, the longest idle first, and their space reused (first fit, merged free ranges). A fork child whose text is mapped after its parent's release still finds its veneer, unless the area filled up in between.
- `relocSnap` takes the veneers of the new sites for the address space; `applySnap` takes those of the state it applies.
- A snapshot copies only its own shared veneers, one piece each, instead of the whole area from `ftop` up: applying a state never writes over a veneer the address space keeps for an unmapped site, or over stale bytes of an earlier owner of a recycled stub. Relocation moves those pieces instead of copying and clearing bytes, and the "shared veneer moves onto another" refusal is gone (the registry never gives two live veneers the same space). Snapshots are up to 224 KiB smaller each.
- New `GVISOR_PROFSTATS` keys: `winstub syscall site: shared veneer area full` (a site that got none), `... shared veneer space reused` (an idle veneer dropped for another).

Not done: keying veneers relative to the region base, so that relocated pieces reuse them. Reclaim alone meets the target, and relative keys would make "a fork parent and child compute the same bases" a correctness condition (bases are only kept with the cache on).

### Test

`TestSharedVeneerReclaim` (windows-platform, all legs; run 46105662196), with the text cache off and the area shrunk to two veneers:
- a thread of A stops in the veneer of a site (a syscall returned into it), and A unmaps the site;
- B gets the last room, C gets no veneer (area full: A's is not taken);
- B is released; D gets B's space;
- A's veneer bytes are unchanged; A maps the site again, gets the same veneer, and the stopped thread continues (`mov %rax,%rdi`, then syscall 501 with rdi 77);
- A is released; E gets A's space.

`TestTextRelocate` (whose oracle compares the used stub area, now per veneer) and `TestTextCache` pass unchanged.

### Numbers (windows-profile dyn-forkexec, run 46105174712; rewrite mode, one sandbox)

| | before (46101653867) | after |
|--|--|--|
| 300x `/bin/true` | 1.34 s run; 596 relocated | 2.61 s run; 596 relocated |
| 1000x `/bin/true` | 6.44 s run; 1016 relocated, 980 refused (area full), 2976 patched at other bases (2775 ms) | 7.43 s run; 1996 relocated (1394 ms), 0 refused, 0 area full; 1996 small pieces patched at other bases (187 ms); 3430 idle veneers reused |
| ms per relocation, 300 / 1000 | 0.36 / 0.37 | 0.71 / 0.70 |

The runner of the new run was about twice as slow (the 1496 same-base hits of the 300 loop took 452 ms against 212 ms; the native 100x `/bin/true` loop 0.86 s against 0.35 s), so the absolute times do not compare across the two runs. Within the new run the cost per exec does not grow: 8.7 ms per exec over 300 and 7.4 ms over 1000 (boot included in both), and every piece is relocated in both. The 100-exec loops of the same run: `/bin/true` 1.04 / 1.69 / 2.56 s in rewrite mode (relocation on / off / cache off), 0.96 / 1.65 / 2.51 s in rewrite-fxsave, the same ratios as 1t.

## 2. Host I/O under locks (items 1, 2, 5 of 1t's table)

- `dropAllCaches` (`fsimpl/gofer/winfs_windows.go`): only the dentry and listing invalidation runs under `renameMu` (`dropAllDentries`); the stats of the root and of every cached inode run after it. `updateMetadata` takes only the inode's own `metadataMu`, as the fstat path does. `TestVFSWatchedOverflowDropsTheCache` now also checks that `renameMu` can be write-locked while the stats run (a test hook).
- `Pin` (`winhost/host_windows.go`): `NtCreateFile` runs outside `pinMu`; of two Pins that open at once, the second counts on the first's pin and closes its handle (outside the lock).
- `pinByName`: a reference taken under `pinMu` keeps the pin's handle open while `handlePath`, the open by name and the ID check run outside it; under the lock again the new handle is installed unless another call switched it first or the pin was released meanwhile; the handle by ID is closed before returning (the replace that follows depends on it). `TestPinConcurrent`: 8 goroutines x 50 Pin, pinByName, Unpin leave no pin and the file readable.
- winstub `MapFile` text path: the file is read before `as.mu` into a pooled buffer, and the cache lookup compares bytes (`equalFile` is gone). The buffer goes back to the pool when the patch comes from the cache; a patched text keeps it (the patch state keeps slices of it).

## Checks

Product code at b78c51c4d; later commits add ADR 0025, the test to windows-platform's list and this file.

| check | result |
|--|--|
| windows-platform, all legs | green, run 46105167374; TestSharedVeneerReclaim in all legs, run 46105662196 |
| windows-fs (TestPinConcurrent, TestVFSWatchedOverflowDropsTheCache) | green, run 46105169950 |
| fs-flakes | green, run 46105172344 |
| windows-sentry-guests | green, run 46105861090 |
| windows-runsc | green in all legs, run 46105864152 |
| windows-stability, 50 runs per leg | 50 of 50 in every leg; busybox fork+exec (median of 7, spares 0 / 2) 0.58 / 0.58 s native, 0.56 / 0.47 s rewrite, 0.46 / 0.46 s rewrite-fxsave, against 0.57 / 0.46 / 0.57 s in 1t (run 46101650677): within the runners' spread; run 46105868277 |
| podman-windows modes=native,rewrite,rewrite-fxsave runs=4 | all 10 steps in all 12 runs (FAILED_STEPS=0), run 46105871393 |
| windows-profile dyn-forkexec | run 46105174712 (above) |

## Open

- An address space that maps and unmaps libraries at new addresses many times keeps their veneers until it exits.
- A fork child whose text is mapped only after the parent was released (`WINSTUB_EAGER=0`, or a failed eager map), at a time the area is full, may find its veneer reused.
- 1t's items 3, 4, 6 to 9 (directory handle close under `dirCache.mu`, metadata stream under `metaMu`, winstub's synchronous log, dump, `freeThread` waits, gofer page faults) are unchanged.
- `pinByName` still closes the handle by ID while an `openID` caller may be using it (as before).
