# 1u: shared veneers reclaimed, host I/O out of Sentry locks

Verdict: shared veneers are now given back when the last address space or cached state holding them goes, so 1000 execs of a dynamic `/bin/true` in one sandbox relocate every piece (1996 of 1996, 0 refused, 0 "area full") at the same cost per relocation as over 300 execs, where before 980 relocations were refused after about 500 execs; and the three host I/O calls named in 1t no longer run under mount-wide or address-space locks.

Nodes: `platform-shared-veneers-fill`, `sentry-host-io-under-locks`. Design: ADR 0025.

## 1. Shared veneers

### Cause

A syscall site whose patch displaces instructions after the syscall returns into its veneer, and a fork child resumes there, so `sharedVeneer` handed out one address per (site address, original bytes) for the whole platform and never took it back (ADR 0022, 0024). Each exec of a dynamic program puts libc at a new address, so each took about 7 more veneers of the 224 KiB area; around exec 500 it was full (1t, run 46101653867).

### Change (`veneers_windows.go`, ADR 0025)

- The registry counts holders per veneer. An address space holds every veneer it got (`as.vheld`) until `Release` (synchronously, before the stub is emptied in the background), whether the site is still mapped or not: a thread stopped in a syscall, or a signal frame, may return into the veneer of an unmapped site, and the platform cannot see either, so unmap gives nothing back. A cached patch state (`patchSnap`) holds the veneers it lists, from `snapshot` to `release`.
- A veneer without holders keeps its address; only when the area has no room left are idle veneers dropped, the longest idle first, and their space reused (first fit, merged free ranges). A fork child whose text is mapped after its parent's release still finds its veneer, unless the area filled up in between.
- `relocSnap` takes the veneers of the new sites for the address space; `applySnap` takes those of the state it applies.
- A snapshot copies only its own shared veneers, one piece each, instead of the whole area from `ftop` up: applying a state never writes over a veneer the address space keeps for an unmapped site, or over stale bytes of an earlier owner of a recycled stub. Relocation moves those pieces instead of copying and clearing bytes, and finds every piece before moving any: a moved veneer can land where another one of the same state was (two sites with the same bytes the region's delta apart get the second one's key), which the old code refused as "shared veneer moves onto another". Snapshots are up to 224 KiB smaller each.
- New `GVISOR_PROFSTATS` keys: `winstub syscall site: shared veneer area full` (a site that got none), `... shared veneer space reused` (an idle veneer dropped for another).

Not done: keying veneers relative to the region base, so that relocated pieces reuse them. Reclaim alone meets the target, and relative keys would make "a fork parent and child compute the same bases" a correctness condition (bases are only kept with the cache on).

### Test

`TestSharedVeneerReclaim` (windows-platform, all legs; run 46105662196), with the text cache off and the area shrunk to two veneers:
- a thread of A stops in the veneer of a site (a syscall returned into it), and A unmaps the site;
- B gets the last room, C gets no veneer (area full: A's is not taken);
- B is released; D gets B's space;
- A's veneer bytes are unchanged; A maps the site again, gets the same veneer, and the stopped thread continues (`mov %rax,%rdi`, then syscall 501 with rdi 77);
- A is released; E gets A's space.

`TestTextRelocateOntoAnother`: the same piece twice, the region's delta apart, mapped again one delta higher; both pieces are relocated, the state equals a fresh patch, and both sites continue in their own veneers.

`TestTextRelocate` (whose oracle compares the used stub area, now per veneer; the comparison is shared with the new test as `compareStates`) and `TestTextCache` pass unchanged.

### Numbers (windows-profile dyn-forkexec, rewrite mode, one sandbox)

| | before (46101653867) | after, first run (46105174712, b78c51c4d) | after, final code (46107583343) |
|--|--|--|--|
| 300x `/bin/true` | 1.34 s run; 596 relocated | 2.61 s run; 596 relocated | 1.75 s run; 596 relocated |
| 1000x `/bin/true` | 6.44 s run; 1016 relocated, 980 refused (area full), 2976 patched at other bases (2775 ms) | 7.43 s run; 1996 relocated, 0 refused, 0 area full | 5.04 s run; 1996 relocated (945 ms), 0 refused, 0 area full; 1996 small pieces patched at other bases (153 ms); 3430 idle veneers reused |
| ms per relocation, 300 / 1000 | 0.36 / 0.37 | 0.71 / 0.70 | 0.49 / 0.47 |
| ms per exec, 300 / 1000 (boot included) | 4.5 / 6.4 | 8.7 / 7.4 | 5.8 / 5.0 |

The runners differ (the native 100x `/bin/true` loop: 0.35 s in the run before, 0.86 s and 0.49 s in the two after), so absolute times do not compare across runs. Within each run after the change, the cost per exec does not grow from 300 to 1000 execs and every piece is relocated, where before it grew by half once the area was full. The 100-exec loops of the final run: `/bin/true` 0.67 / 1.17 / 1.79 s in rewrite mode (relocation on / off / cache off), 0.65 / 1.16 / 1.80 s in rewrite-fxsave, 0.49 / 0.59 / 0.75 s native, the same ratios as 1t.

## 2. Host I/O under locks (items 1, 2, 5 of 1t's table)

- `dropAllCaches` (`fsimpl/gofer/winfs_windows.go`): only the dentry and listing invalidation runs under `renameMu` (`dropAllDentries`); the stats of the root and of every cached inode run after it. `updateMetadata` takes only the inode's own `metadataMu`, as the fstat path does. An inode collected for the refresh may now be destroyed while the stats run (destruction of dentries waited for the read lock before); that is harmless, as it already was for regular-file inodes released by a file close: the stat is by file ID and only updates the dead inode's fields, and `DropAll` under `dataMu` finds the cache emptied by `destroy`. `TestVFSWatchedOverflowDropsTheCache` now also checks that `renameMu` can be write-locked while the stats run (a test hook).
- `Pin` (`winhost/host_windows.go`): `NtCreateFile` runs outside `pinMu`; of two Pins that open at once, the second counts on the first's pin and closes its handle (outside the lock).
- `pinByName`: a reference taken under `pinMu` keeps the pin's handle open while `handlePath`, the open by name and the ID check run outside it; under the lock again the new handle is installed unless another call switched it first or the pin was released meanwhile; the handle by ID is closed before returning (the replace that follows depends on it). `TestPinConcurrent`: 8 goroutines x 50 Pin, pinByName, Unpin leave no pin and the file readable.
- winstub `MapFile` text path: the file is read before `as.mu` into a pooled buffer, and the cache lookup compares bytes (`equalFile` is gone). The buffer goes back to the pool when the patch comes from the cache; when the text is patched (in place, in the buffer), it is left to the garbage collector. Patching keeps only copies of it (tails, heads, split bytes, the scan cache), so a pool buffer larger than the text is not pinned.

## Checks

Final product code at 8b340cb87 (later commits change only this file).

| check | result |
|--|--|
| windows-platform, all legs (TestSharedVeneerReclaim, TestTextRelocateOntoAnother, TestTextRelocate, TestTextCache, TestRecycle) | green, run 46107580466 |
| windows-sentry-guests | green, run 46107586378 |
| windows-runsc, all legs | green, run 46107588960 |
| windows-stability, 50 runs per leg | 50 of 50 in every leg; busybox fork+exec (median of 7, spares 0 / 2) 0.56 / 0.56 s native, 0.66 / 0.46 s rewrite, 0.51 / 0.46 s rewrite-fxsave ("all on" 0.57 / 0.46 / 0.46 s), against 0.57 / 0.46 / 0.57 s in 1t (run 46101650677); run 46107591253 |
| podman-windows modes=native,rewrite,rewrite-fxsave runs=4 | all 10 steps in all 12 runs (FAILED_STEPS=0), run 46107594309 |
| windows-profile dyn-forkexec | run 46107583343 (above) |
| windows-fs (TestPinConcurrent, TestVFSWatchedOverflowDropsTheCache), at b78c51c4d | green, run 46105169950 |
| fs-flakes, at b78c51c4d | green, run 46105172344 |

The commit after b78c51c4d changes only `relocSnap` and winstub tests, which windows-fs and fs-flakes do not run. At b78c51c4d the other suites were green too: windows-platform 46105167374, windows-sentry-guests 46105861090, windows-runsc 46105864152, windows-stability 46105868277, podman-windows 46105871393 (FAILED_STEPS=0 in all 12 runs).

## Open

- The area is now bounded by what is held at once: 3000 to 4600 veneers of 48 to 80 bytes in 224 KiB, against about 7 per exec of a dynamic program (libc) held by its live address space, plus those of cached states. Hundreds of concurrent dynamic processes, or many distinct cached library layouts, could still fill it; sites then go to the debugger as before.

- An address space that maps and unmaps libraries at new addresses many times keeps their veneers until it exits.
- A fork child whose text is mapped only after the parent was released (`WINSTUB_EAGER=0`, or a failed eager map), at a time the area is full, may find its veneer reused.
- 1t's items 3, 4, 6 to 9 (directory handle close under `dirCache.mu`, metadata stream under `metaMu`, winstub's synchronous log, dump, `freeThread` waits, gofer page faults) are unchanged.
- `pinByName` still closes the handle by ID while an `openID` caller may be using it (as before).
