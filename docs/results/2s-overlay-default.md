# 2s: native overlay diff, overlay in all modes, overlay as the default storage driver

Branch `fs-overlay-default`.

Verdict: c/storage's native-diff probe failed because the Sentry forced `userxattr` for a host-folder upper layer, so a lower layer's `trusted.overlay.opaque` was an ordinary xattr and was copied up; with that fixed on Windows, `podman info` reports Native Overlay Diff true and the acceptance passes all 10 steps in 24 of 24 runs (native, rewrite and rewrite-fxsave, 8 each) with the overlay driver, which is now the default (ADR 0021).

## Cause

`overlay.FilesystemType.GetFilesystem` enables `userxattr` whenever the upper layer is a gofer ("9p") filesystem, because a real gofer cannot set `trusted.*` on the host. The overlay then only treats `user.overlay.*` as private. c/storage's `doesSupportNativeDiff` marks `l2/d` with `trusted.overlay.opaque=y`, mounts without `userxattr`, writes `merged/d/f` and checks that `l3/d` (upper) has no such xattr. In the Sentry `trusted.overlay.opaque` was not private, so `copyXattrsLocked` copied it up: "opaque flag erroneously copied up". The Windows host-folder backend (winhost) stores xattrs of any namespace itself (`checkXattrPermissions` already allows `trusted.*` there), so the forced option has no reason on Windows.

## Reproduction and fix

- Guest `test/guests/ovlopaque.c` (`runsctest -only ovlopaque`, step in `fs-flakes.yml`) mirrors the probe on a host-folder bind mount. Before the fix: `FAIL trusted.overlay.opaque: opaque flag erroneously copied up (value y)` in native, rewrite and rewrite-ud2 (run 46048295707, head e37e985b1).
- Fix (`pkg/sentry/fsimpl/overlay/overlay.go`, marked `gvisor-windows:`): no forced `userxattr` on Windows; the overlay keeps `trusted.overlay.*` there, as on Linux. An explicit `userxattr` mount option still works.
- fs-flakes with the fix: native, rewrite, rewrite-ud2 green, `ovlopaque` included (run 46048850603). windows-fs green (46048854650).

## Overlay in all modes

`ci-run.ps1` accepts a `-overlay` suffix on any mode (`native-overlay`, `rewrite-overlay`, `rewrite-fxsave-overlay`), and after the engine starts logs `podman info` graph status; with the overlay driver the run fails if Native Overlay Diff is not true. (No new workflow input: modes are free text.)

Run 46048940236 (`-f modes=native-overlay,rewrite-overlay,rewrite-fxsave-overlay -f runs=8`, head 34ad8fee6): 24 of 24 acceptance jobs green (the workflow requires all 10 steps), each logging `graph: overlay: map[... Native Overlay Diff:true ...]`.

## Default switch (ADR 0021)

`start-engine.ps1` and `ci-run.ps1` default to `overlay` (fuse-overlayfs is removed from the engine's writable root whenever the driver is overlay), the portable zip's `podman-engine.ps1` gets `-StorageDriver overlay|vfs`, `README-release.md` and the ADR 0002 conditions point to ADR 0021. `ci-run.ps1` modes with a `-vfs` suffix select vfs. Verification of the default: run 46051410093 (default mode, runs=2, head dc430af89).

## Open

- Storage folders made with vfs cannot be reused with overlay (delete the folder or pass `-StorageDriver vfs`); the release zip's clean-machine test was not rerun here.
- The docs/roadmap and tech-tree were not touched (instruction).
