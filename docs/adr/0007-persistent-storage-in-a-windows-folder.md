# 0007. Persistent storage in a Windows folder

- Status: Accepted (decided under the owner's delegation)
- Date: 2026-11-26

## Context
gVisor's `--overlay2` media do not persist across restarts; a store on a host directory did (Phase 0a.4, with overlay and vfs drivers). The options were NTFS files with Linux metadata, one large filesystem image (needs a writable on-disk filesystem in the Sentry, which gVisor lacks), or an in-memory store with a cache.

## Decision
Persistent data lives as real files in a Windows folder per sandbox, served by the Windows directfs backend (ADR 0005). Linux metadata NTFS cannot hold natively (owner, mode, device nodes, xattrs, overlay whiteouts) is kept in an NTFS alternate data stream per file; hard links and symlinks use NTFS's own where the user may create them, otherwise they are emulated in that stream.

## Consequences
- No new on-disk filesystem in the Sentry; one backend serves both bind mounts and storage.
- The metadata stream is the piece that must be right for container images (whiteouts, device nodes); it gets its own tests.
- Speed is bounded by NTFS metadata operations; the in-memory overlay stays available for scratch data.
