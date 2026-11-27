# 2r: step 5 (build with apt) in native mode, and native overlay on a host folder

Branch `fs-step5-flake`. Runs are `podman-windows` dispatches (`-f only=... -f modes=... -f runs=N`) and `fs-flakes` dispatches.

Verdict: step 5 does not fail on the current `windows` (0 of 80 native runs; both failures come from builds that predate the mount-keeping and rename fixes and were not reproduced by a guest test), and native overlay on a host folder now passes all 10 steps in 4 of 4 runs after one fix: the host-folder backend listed whiteouts (emulated character devices) as DT_UNKNOWN, so the Sentry's overlay treated them as files.

## Task 1: step 5 intermittent failures

Failing runs: 45980964285 native-1 (head `c39bdbdd1`, `apt-key: cannot create /dev/null: Permission denied`, for every apt-key call of the build) and 45980957712 (dpkg `cannot stat '/tmp/apt-dpkg-install-.../*.deb'`). Neither head contains `invalidate_mounts.go` (757d3cef7, mounts survive invalidation) or the rename fixes (415d1aa64 and earlier on fs-rename); both are in `windows` since `d502cc794`.

**Cause of (a).** The failure is per container (every apt-key of one build fails), so it is set up when crun builds the container, not a race in `open`. In a tmpfs `/dev`, a missing `/dev/null` makes sh's `> /dev/null` (O_CREAT) fail for `_apt` with EACCES from the parent directory check, exactly as in the log; root never notices (DAC override). That state is a `/dev` (or `/dev/null` bind) mount that is gone: the Sentry dropped it when the host folder's change stream overflowed (`dropAllCaches`, 3 to 4 per acceptance run) and `vfs.InvalidateDentry` unmounted what sat on an invalidated gofer dentry. That is the bug the `windows` branch fixed with `invalidate_mounts.go`; the earlier symptom there was `crun: open /dev/null: No such file or directory`. Not shown directly: the old logs have no overflow line (the log was added later), so this attribution rests on the log analysis and the dates of the fixes.

**Cause of (b).** Undetermined. The logs of 45980957712 were not opened. The dpkg directory symlinks to archives apt had just renamed from `partial/`, and the failing head predates the whole fs-rename merge (atomic replace, by-name pins, mounts kept; docs/results/5-podman-windows.md, "Rename atomicity"); which of those fixed it is not known, and scripts never reproduced it before either.

**Reproducer.** `test/guests/devnull.c` (`runsctest -only devnull`, run by `fs-flakes`): tmpfs on `/w/dn/c/dev` and `/w/dn/c/tmp` in a host-folder mount, mknod 1:3 mode 0666, then per round a child does `setgroups(0)`, `setgid`, `setuid(100)`, `execv` of a helper that opens the node `O_WRONLY|O_CREAT|O_TRUNC`, downloads into `archives/partial` and renames into `archives`; root symlinks it from `/tmp/apt-dpkg-install-N/` and stats through the link, while a second process renames and lists other names; Windows floods the folder meanwhile (`startFlood`). 1000 rounds x 3, native and rewrite: 0 failures, on the fixed tree and on a branch with the mount keeping disabled (the flood does not overflow the stream while the loop runs; one overflow came at teardown). So it did not reproduce the failure and is a regression guard only.

**Diagnostics added.** `ci-run.ps1` modes `native-probe` (step 5 runs `stat /dev/null`, `su _apt -c 'echo > /dev/null'` and the mounts before apt, and the same plus `/tmp`, archives and the symlink targets when apt fails) and `native-diag` (also `runsc --strace` of the file-system and credential calls); `vfs.InvalidateDentry` logs a warning with a stack when a mounted dentry is dropped (it did not appear in the engine logs of the 30 probe runs, the only ones searched; the 20 diag and 30 native runs passed step 5, their logs were not searched).

**Measurements.** 20 runs (10 probe, 10 diag; run 45988452811), 30 probe runs (45990649858), 30 plain `native` runs on the final branch (45993699931, step 5 only): 80 of 80 pass. Step 5 takes 144 to 346 s (median 218 s) with vfs in the 30 parallel `only=5` jobs; the 4 native-overlay jobs (full runs, 4 in parallel) took 34 to 70 s, so the two figures are not a like-for-like comparison. Earlier heads with the fixes also passed: 8 of 8 native on 71ad200fc (45985526511). The engine logs of the 30 probe runs show the overflow and `invalidating around the mounts` lines, so the fixed path is exercised.

## Task 2: native overlay on a host folder

c/storage's `supportsOverlay` probe failed with `unlinkat .../merged/subdir: directory not empty`. Reproduced by `test/guests/ovlsub.c` (`runsctest -only ovlsub`, `fs-flakes`): lower2 has `subdir/file`, a character device 0:0 `subdir/file` hides it from lower1, from the upper layer, or the overlay itself creates it by unlink; all on a host-folder bind mount; `merged/subdir` must list empty and be removable. Before the fix (run 45990166167): `merged/subdir lists 1 entries`, `unlink` of that entry ENOENT, whiteout shown as `file(type 0)`.

**Cause.** winhost keeps device nodes as files with the type in an alternate data stream and its directory listing does not read it, so `ReadDir` returned `DT_UNKNOWN` for them. The Sentry overlay recognises a possible whiteout only by `DT_CHR` (`directory.go`: readdir merge and `collectWhiteoutsForRmdirLocked`), so an unknown entry was a regular file: listed, not removable (the lookup does treat the node as a whiteout), and it made rmdir fail with ENOTEMPTY.

**Fix.** `winInode.hostGetDirentsLocked` (`winfs_windows.go`) looks up the type of entries with `DT_UNKNOWN` (system-attribute files only) after the listing. `ovlsub` then passes in native and rewrite (run 45990815006; 45992984595 on the final branch).

**native-overlay.** `-f modes=native-overlay -f runs=4` (run 45992758965): all 10 steps pass in 4 of 4 runs, `STORAGE_DRIVER=overlay` in the engine environment, `podman info` reports `graphDriverName: overlay` with no mount_program, restart keeps 4 images; c/storage logs `Not using native diff for overlay ... opaque flag erroneously copied up` (Native Overlay Diff false: a remaining Sentry difference, slower layer diffs, not a failure). fuse-overlayfs is now removed through the engine environment (`ENGINE_NO_FUSE_OVERLAYFS=1`, set by the `native-overlay` mode, `rm` in the engine's writable root before podman starts) and no longer in the image (the cache key is `podman-erofs-v3`).

## Open

- The default storage driver of the acceptance is still `vfs`; ADR 0002 ("Storage driver vfs") would need a superseding decision to change it. Not done here.
- c/storage's native-diff probe ("opaque flag erroneously copied up") fails in the Sentry's overlay, so layers are diffed the slow way; not investigated.
- A throwaway branch `fs-step5-revert` (mount keeping disabled, run 45992962972) was pushed for the revert check and deleted.
- Not run: native-overlay with more than 4 runs, rewrite mode with overlay.
- `aptrename` still crashes in rewrite mode only (`free(): invalid size`), the platform owner's defect.
- The vanished-mount attribution of (a) and the rename attribution of (b) are inferred; a failure on a current head would show up in the probe (`native-probe` prints the state at failure).
