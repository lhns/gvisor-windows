# 0015. Sandbox root filesystem as an erofs image

- Status: Accepted (decided under the owner's delegation)
- Date: 2026-11-26
- Refines: ADR 0007

## Context
A sandbox needs a root filesystem (for the acceptance bar: a Linux userland with Podman, crun and netavark) with symlinks, modes, owners and device nodes. Writing such a tree onto NTFS needs an importer that creates the metadata streams of ADR 0007 for every file. gVisor already supports read-only erofs images as a root filesystem (`pkg/erofs`, `fsimpl/erofs`, `runsc/boot/vfs.go`); it maps the image read-only, which on Windows is a file mapping instead of `mmap`. Images Podman pulls *inside* the sandbox are written by the guest through the Windows host-folder backend, which already stores the metadata streams.

## Decision
- The sandbox root filesystem is an erofs image built from an OCI image on Linux (`mkfs.erofs` in CI) and shipped as one file, with gVisor's overlay on top (memory-backed upper layer for scratch changes).
- Persistent data (Podman's storage, volumes, bind mounts) lives in Windows folders served by the host-folder backend (ADRs 0005, 0007).
- No importer for unpacking image trees onto NTFS.

## Consequences
- `pkg/erofs` needs a Windows file mapping for its image; the rest of erofs works unchanged.
- Changes to the root filesystem do not survive a restart, matching a container's root; anything that must persist goes to a host folder.
- Building the root filesystem image needs Linux tooling, which CI provides.
