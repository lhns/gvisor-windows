# 0005. Host files accessed directly, no file protocol

- Status: Accepted (owner decision)
- Date: 2026-11-26

## Context
On Linux, gVisor reaches host files through a gofer process (lisafs protocol, Linux-only transport) or, in directfs mode, opens them itself. remote-docker serves files over NFSv3 because the files sit on another machine and Linux has an in-kernel NFS client. Here the Sentry is the kernel and runs on the same machine as the files; gVisor has no NFS client.

## Decision
A Windows directfs backend: the Sentry opens NTFS files itself (NtCreateFile and friends), serves reads from its own dentry and page cache and invalidates via `ReadDirectoryChangesW`. The Sentry is a compatibility layer with the user's own rights, not a security boundary against the user's account. Containers stay isolated from each other.

## Consequences
- No 9P, lisafs or NFS hop; consistency and caching semantics are taken from remote-docker (ADR 0042/0044 there), not its transport.
- Handles are held briefly and files are not mapped as sections, so editors and git can still rename and delete them.
- Where Podman's image store lives across restarts is still open (gate 0a.4).
