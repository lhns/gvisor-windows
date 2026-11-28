# 1t: patched-text cache for PIE and dynamic programs, wait status flake, host I/O under locks

Verdict: an exec of a dynamic or PIE program now takes its patched text from the cache, relocated to the new addresses. In the same job, 100 fork+exec of Debian's dynamic `/bin/true` from bash took 0.89 to 0.92 s instead of 1.52 to 1.55 s in rewrite mode, and 0.79 s instead of 0.87 s natively. The gain stops after about 500 such execs in one sandbox, when the shared veneer area is full (see Open). Separately, a fault-path bug had kept every dynamic program's libraries out of the cache. `sh_wait_status` failed because the guest root had no `/dev/null` (a race in the test, not in the Sentry); it now passes 30 of 30 in every leg.

Nodes: `platform-text-cache-pie`, `sentry-sh-wait-status-flake`, `sentry-host-io-under-locks`. Design: ADR 0024, which supersedes ADR 0022's cache key.

## 1. Text cache for PIE and dynamic programs

### Measured before

New job `dyn-forkexec` of windows-profile. It runs `sentryguest` with Debian trixie's root (bash, coreutils, python3) and a static PIE built with `gcc -static-pie`, in all three legs. Each loop is a median of 5 runs, boot included. New `GVISOR_PROFSTATS` keys `winstub text: ...` count each text map by outcome, with its time.

Baseline, run 46097629649 (code as on `windows`):

| loop | native | rewrite | rewrite-fxsave |
|--|--|--|--|
| bash, 100x `/bin/true` (PIE, dynamic) | 0.87 s | 1.52 s | 1.53 s |
| bash, 20x `python3 -c pass` | 0.85 s | 1.18 s | 1.17 s |
| bash, 100x static PIE | 0.50 s | 0.77 s | 0.74 s |
| busybox sh, 100x `busybox true` (static) | 0.32 s | 0.43 s | 0.40 s |

In the `/bin/true` loop, 103 of the 413 text maps were patched with the cache off for the address space. In rewrite mode they cost 942 ms of the 1.5 s.

### Cause 1: an exec fault ended the history

`wxExec` is called on every instruction-fetch fault. It cleared `histOK` (the address space leaves the cache for good) before checking that the view really was writable and executable. A dynamic program's libraries are mapped by ld.so and fault in on their first fetch, so libc and everything after it were patched without the cache in every exec, fork children of a dynamic shell included. `histOK` is now cleared only for a real W^X view. On a faster runner (run 46098476549), the `/bin/true` loop then took 0.58 s native and 0.89 s rewrite, with 0.63 and 1.42 s with the cache off.

### Cause 2: absolute addresses in the key

What was left were the pieces at random addresses: the PIE executable (low region, `arch.PIELoadBase` plus up to 256 MiB) and the mmap area (vDSO, ld.so, libc and dlopen'd libraries). The trace of two execs shows that each region moves as a whole: ld.so and libc were 0x1D4000 apart, and the vDSO and ld.so 0x3000 apart, in both. The executable moves by its own delta.

### Change (ADR 0024)

- `textcache_windows.go`: the history and key are relative to the first text of each region (below 4 GiB, above), and an entry keeps its bases. Same bases: the old path, unchanged.
- `textreloc_windows.go`: an entry made at other bases is relocated by each region's delta. Patching now records what depends on the place:
  - guest addresses in the stub area (`absAt`): the trap entries' return address, a veneer's window end, a relocated rip-relative target, each tagged with the region of its code;
  - rel32 jumps from the text (`relAt`, with their target): the low region only, mapped as a fixed-up copy of the cached section;
  - shared veneers (`shared`): moved to the address `sharedVeneer` gives the new site, with their own continuation address and trap pointer fixed.

  Map keys and values move by region, and stub addresses stay, except inside a moved shared veneer.
- A piece is relocated only when every low-region piece reaches the stub area with rel32 before and after, and every high-region piece is out of reach, so patching would choose the same jumps. Pieces under 64 KiB are patched (relocating copies the whole state; the vDSO costs 0.02 ms to patch). A piece across both regions, and fixHead, put the bases into the history.
- `WINSTUB_TEXTRELOC=0` turns relocation off.

Shared veneers were the one part that could have forced a refusal: `/bin/true`'s libc and the static PIE have some (about 7 per exec, counter `winstub syscall site: shared veneer`). Relocation moves them instead of refusing.

### Tests

`TestTextRelocate` (windows-platform, all legs; run 46100324148 and the final run):
- Setup: a 64 KiB piece in each region with an imm site, a site returning into the text, one returning into a shared veneer, and, in rewrite mode, an `%fs` window that displaces a rip-relative load. Both pieces are patched in two address spaces, then mapped elsewhere in a third, each region by its own delta.
- Oracle: the third space's state must equal a fourth's that patched at the new places with the cache off: text bytes, used stub area, cursors, every address-keyed map, the `%fs` veneers, and the three relocation lists.
- Also: the relocated space runs the syscalls; a fifth space continues a thread stopped in the shared veneer (a fork); and a piece out of rel32 reach is patched, not relocated.

`TestTextCache` is unchanged and passes.

### Numbers (final product code, run 46100560727; same job, relocation on / off / cache off)

| loop | native | rewrite | rewrite-fxsave |
|--|--|--|--|
| bash, 100x `/bin/true` | 0.79 / 0.87 / 1.11 s | 0.92 / 1.52 / 2.67 s | 0.89 / 1.55 / 2.35 s |
| bash, 20x `python3 -c pass` | 0.89 / 0.89 / 1.02 s | 1.23 / 1.24 / 1.52 s | 1.20 / 1.23 / 1.50 s |
| bash, 100x static PIE | 0.61 / 0.55 / 0.87 s | 0.71 / 0.80 / 1.66 s | 0.72 / 0.83 / 1.68 s |
| busybox, 100x static | 0.37 / 0.37 / 0.79 s | 0.49 / 0.46 / 1.70 s | 0.48 / 0.46 / 1.69 s |

Run 46099818950 (same product code apart from tracing) gave the same picture: `/bin/true` 0.73 / 0.83 native and 0.90 / 1.55 rewrite.

Per 100-exec `/bin/true` loop, rewrite mode:
- 196 pieces relocated in 121 ms (0.6 ms each);
- 17 patched the first time (the first two execs);
- 196 small pieces (the vDSO, the 20 KB executable) patched at other bases in 21 ms;
- no relocation refused in any loop.

The native static PIE loop does not gain (0.61 / 0.55 s here, 0.50 / 0.51 s in run 46099818950): natively a relocated hit costs about what patching with a warm scan cache does (0.6 ms), and only rewrite mode pays for placing veneers.

Debian's python3 is not a PIE, so its executable already hit. Its libraries are now relocated, about 1 ms each (90 in 20 execs), but the loop gains little: the first exec patches python3's 3 MB of text (178 ms natively), and the libraries it loads are small. The static busybox loop is unchanged within the runners' noise.

## 2. sh_wait_status: `wait $p` = 2

Reproduced with a new `iterations` input of windows-sentry-guests and a step that loops in one shell and keeps `-strace` (new `sentryguest` flag) output of failing runs. Run 46097632794 (before):
- `sh_wait_status` passed 26, 29 and 29 of 30 in native, rewrite and rewrite-fxsave;
- `sleep 30 & kill; wait` gave 2 instead of 143 in 83, 10 and 14 of 200 tries.

The strace of a failing run: busybox ash, starting a background job in a non-interactive shell, redirects its stdin from `/dev/null`. The tmpfs root of `sentryguest` has no `/dev`, so the child wrote `can't open '/dev/null'` and called `exit_group(2)` before the parent's `kill` arrived. Faster fork+exec (1r) made the child win that race more often. The Sentry's wait status was right. Fix:
- the case runs with `-sysmounts` (devtmpfs with `/dev/null`);
- `wait.sh` redirects to `/dev/null` again.

Run 46098017612: 30 of 30 in every leg, 0 of 200 in the loop, 0 of 30 strace runs failed.

## 3. Host I/O under Sentry locks (investigation)

Since 3e, `pkg/log` writes asynchronously, so log calls under the winhost and gofer locks no longer touch the disk. What is left (file:line as of this branch):

| # | where | lock held | host call | how hot | risk |
|--|--|--|--|--|--|
| 1 | `fsimpl/gofer/winfs_windows.go` `dropAllCaches` (689-726) | `renameMu.RLock`, mount-wide | one open, query and close per cached inode (up to 200k) | rare: the watch overflowed or ended | high when it happens: seconds, and every walk on the mount waits behind a pending rename's writer lock |
| 2 | `gofer/winhost/host_windows.go` `Pin` (182), `pinByName` (205-217), `Unpin` (228) | `pinMu`, mount-wide, taken by every host file op | NtCreateFile, CloseHandle of a written file (a filter driver can scan on close) | unlink or rename over an open file, inode destroy | medium |
| 3 | `winhost/fast_windows.go` `dropLocked` (112-121) | `dirCache.mu`, mount-wide | CloseHandle of a directory | per lookup sweep | low |
| 4 | `host_windows.go` SetAttr (971-989), `meta_windows.go` 161/206, `shadow_windows.go` 130 | `metaMu`, mount-wide | read-modify-write of the metadata stream | chmod, chown, xattr | low (serializes them across files) |
| 5 | `platform/winstub/aspace_windows.go` `MapFile` text path | `as.mu`, which `Switch` takes on every syscall return (`restartTarget`) | reads the file (`equalFile`, `read()`): page faults on a cold erofs image | every text map | medium: tens of ms for MB-sized text, other threads of the process stall |
| 6 | winstub's standard-library `log.Printf` (`removeRange`, `Unmap`, tripwire, debug loop) | `as.mu`, `t.stopMu` | synchronous write to stderr | error, tripwire and debug paths only | low |
| 7 | `dump_windows.go` `DumpState` | `liveMu`, `as.tmu` | Fprintf, a 300 ms sleep | only on a state dump | low |
| 8 | `process_windows.go` `freeThread` (196-224) | `as.relMu` | WaitForSingleObject, 2 and 5 s timeouts | thread exit without reuse | low |
| 9 | gofer `regular_file.go` page faults, reads and writes (393-462, 775, 803) | `inode.dataMu` and mm locks | open by ID, read, close per access | per fault or file op | medium; a per-inode handle cache would revisit docs/results/2-windows-directfs.md (a decision, not a fix) |

Fixed here (trivial): `Unpin` closes the handle after releasing `pinMu`. Not changed:
- Sending winstub's standard log to the asynchronous log would also fix #6, but runsctest's `-winstub-debug` and `-fslog` read those lines from runsc's stderr.
- #1, #2 (`Pin`, `pinByName`) and #5 are small changes worth making next. #5 means reading the text before taking `as.mu`, and comparing bytes instead of calling `equalFile`.

## Checks on the final code

Final product code at dd15f273e (which also stops relocating a state that fixHead patched in place; later commits change only docs).

| check | result |
|--|--|
| windows-platform, all legs (TestTextRelocate, TestTextCache, TestRecycle) | green, run 46101634737 |
| windows-sentry-guests, all legs | green, run 46101644666 |
| windows-runsc, all legs | green, run 46101640958 |
| podman-windows modes=native,rewrite,rewrite-fxsave runs=4 | all 10 steps in all 12 runs (FAILED_STEPS=0), run 46101647979 |
| windows-stability, 50 runs per leg | 50 of 50 in every leg; busybox fork+exec 0.57 / 0.46 / 0.57 s (native / rewrite / rewrite-fxsave), as in 1r and 1s; run 46101650677 |
| windows-profile dyn-forkexec | run 46101653867: `/bin/true` loop 0.35 / 0.45 s native, 0.53 / 0.92 s rewrite and 0.54 / 0.93 s rewrite-fxsave (relocation on / off) on a faster runner |

## Open

- Shared veneers (the ones a syscall returns into) are allocated per site address and never freed, so every exec at a new address takes more of the 224 KiB area. This predates this work. Measured in run 46101653867, rewrite mode, one sandbox:
  - 300 execs of `/bin/true`: 1.68 s, and all 596 relocations succeed;
  - 1000 execs: 6.76 s. After about 500 execs the area is full, and 980 relocations are refused ("shared veneer area full"). The pieces are then patched again (2976 patches, 2.8 s), and the shared sites in them go to the debugger path.
  
  Reclaiming the veneers of sites that no address space maps any more (refcounted per site), or keying them by the site relative to its region, is the next step. It should come before long-lived sandboxes that run many dynamic programs.
- The relocation lists must be kept by every patch kind that embeds an address. `TestTextRelocate` covers the current kinds.
- A text unmap whose range extends past the text (the memory manager widens unmaps to the gaps) is recorded relative to the base, so its end differs per exec, and later pieces miss. This was not seen in these loops (no such unmap in their traces).
