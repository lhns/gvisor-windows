# 0021. Overlay storage driver for the Podman engine

- Status: Accepted
- Date: 2026-11-27
- Amends: the "Storage driver `vfs`" condition of ADR 0002 (Conditions of the pass); 0002's decision is unchanged

## Context
ADR 0002 accepted the Podman pass with `vfs` because c/storage's overlay probe failed on the Sentry's overlay over a host folder. Two Sentry differences caused it, both fixed (docs/results/2r-step5-overlay.md, docs/results/2s-overlay-default.md): whiteouts listed as `DT_UNKNOWN` by the host-folder backend, and `userxattr` forced for a gofer upper layer, which made a lower layer's `trusted.overlay.opaque` an ordinary xattr that was copied up (c/storage's native-diff probe, "opaque flag erroneously copied up"). The Linux harness already uses overlay; `vfs` copies every layer in full, so it is slow and large.

## Decision
The engine's storage driver is `overlay` (`STORAGE_DRIVER=overlay`, fuse-overlayfs removed from the engine's writable root because its FUSE daemon cannot run in the sandbox). `start-engine.ps1`, `ci-run.ps1` and the portable zip's `podman-engine.ps1` default to it; `-StorageDriver vfs` (acceptance modes with a `-vfs` suffix) keeps vfs selectable. On Windows, an overlay upper layer on a host folder keeps `trusted.overlay.*` xattrs (winhost holds any namespace).

## Consequences
- c/storage reports Native Overlay Diff true; the acceptance fails if it does not.
- A storage folder made with vfs cannot be reused with overlay (and the reverse): delete it or pass `-StorageDriver vfs`.
- Layers live as directories with whiteout nodes and xattrs in NTFS alternate data streams on the storage folder; the Windows host folder must keep supporting them.
- Evidence and the result of the 24 runs: docs/results/2s-overlay-default.md.
