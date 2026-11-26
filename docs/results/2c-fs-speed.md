# 2c. Faster host-folder walks, xattrs in erofs images, persistent safe-save memory

Nodes: "Faster stat walks on host folders" (fs-attribute-cache), erofs inodes with xattrs, persisted safe-save metadata. All on `windows-latest` / `ubuntu-latest`; nothing run locally.

CI on the final code commit: windows-fs https://github.com/lhns/gvisor-windows/actions/runs/45891390073, windows-rootfs https://github.com/lhns/gvisor-windows/actions/runs/45891390284 (both green; the other workflows of that push are green too).

## 1. Attribute and dentry cache driven by the watcher
- Default for a host folder is now the write-through cache mode with the ReadDirectoryChangesW watcher: cached dentries, attributes, directory listings and "does not exist" answers stay valid until a change event drops or refreshes them. Without the watch (`NoWatch`, or the watch fails) or with `Revalidate` the old behaviour stays (every path component is looked up again). `Exclusive` keeps the write-back cache. The dentry cache holds 200000 entries (was 1000, which evicted a 20000-file tree). A Windows-created file is found by a cached "does not exist" answer in under a millisecond to 40 ms.
- Own-change recognition no longer uses a time window. Creating/linking/unlinking/renaming leave a token (kind, name) that the one matching Added/Removed/Renamed event consumes; writes, attribute and xattr changes record the file's state after the handle is closed (last write time, size) and a Modified event is ours only if the file still is in exactly that state. An event can arrive before the writer has recorded its state, so an unmatched Modified is looked at again after 40 ms. Test: a Windows write immediately after a guest write is seen after 41 ms (host layer and through the VFS). Weak points: a lost event of ours leaves a token that hides one later event of the same name (expires after a minute); a Windows write that leaves the same time and size is missed.
- USN journal: not used. It needs a volume handle (`\.\C:`), which needs administrator rights for ordinary users, and the CI runner is elevated so it could not show otherwise.
- Lookup got cheaper: on NTFS one `NtQueryInformationFile(FileAllInformation)` replaces three queries (the file ID is the index number, checked implicitly by every ID comparison of the tests), and a directory opened for lookups is kept for 200 ms (handles share delete; dropped at once when the backend removes or renames anything) so a directory walk opens it once.

Numbers (windows-latest, 20000 files of 2 KB in 400 directories, through the VFS, per file or per stat; two runs):
| | untar | stat walk, warm (dentries cached) | stat walk, cold (fresh mount) |
|---|---|---|---|
| before (shared, revalidating) | 1.26-1.49 ms | 120-200 us | |
| watched write-through cache (default now) | 1.29-1.41 ms | 1 us | 99-105 us |
| revalidating mode now | 1.41-1.52 ms | 190 us | 197-201 us |
| exclusive | 1.32-1.64 ms | 1 us | 96-112 us |
| plain os calls | 0.69-1.08 ms | 22-30 us (lstat) | |
Done criterion: the warm walk (the case of a sandbox that has seen the tree, like repeated `git status`) is 20x faster than native, far inside 2x; untar is no worse than before. The cold walk (every file looked up for the first time: open, query, metadata-stream probe, dentry creation) is about 4x native and was not brought inside 2x; the remaining cost is the per-file open plus the probe for the metadata stream. A directory-listing prefill cannot supply the link count, so it was not built.

## 2. erofs images with xattrs
`pkg/erofs` no longer rejects inodes with xattrs: it reads the inline xattr area and the shared xattr area (entries with name index user., trusted., security., and the two POSIX ACL names; entries with a long name prefix are skipped) and the inline data offset now accounts for the area. The erofs filesystem answers `getxattr`/`listxattr` (read permission and namespace rules as for any file). The CI image `synth` gets, via `setcap`/`setfattr`: `security.capability` on a binary, `security.selinux` on it and on 50 files with the same label (stored in the shared area), `user.*` on a file, an empty `user.*`, six `user.key*` on a directory (several inline entries) and a `trusted.*`; Podman's image carries two capability attributes (restored with `tar --xattrs`). Through the VFS and the overlay, 63 attributes of synth and 2 of podman read back byte for byte and appear in `listxattr` (0 wrong). The earlier checks of the three images still pass.

## 3. Safe-save memory persisted
The (directory, name) to metadata memory of 2b is written to an append-only log in the stream `:gvisor.index` of the folder's root directory (records: put or delete, directory ID, name, file ID, metadata; batched once a second and at close, read at mount, rewritten when it holds more than twice as many records as live entries). The memory also follows chmod, chown and xattr changes (before, a replacement after a chown restored the owner from creation: found by `TestVFSSafeSaveKeepsMetadata`). Test: create with owner, mode and xattr, close, replace the file by rename from outside while nothing is mounted, remount: mode, owner and xattr are back; a new file at a name the guest had removed gets defaults.

## Open problems
- Cold stat walks are about 4x native.
- A lost watcher event leaves the cache stale (overflow only evicts unreferenced dentries); there is no periodic revalidation.
- The persisted memory covers files the guest looked at or created; the log write is lost for the last second on a crash.
- In write-through mode guest times come from the guest clock until the next refresh; a Windows write that leaves identical time and size is missed.

Verdict: a watched host folder now answers a repeated stat walk of 20000 files from cache at 1 us per file (native 22-30 us) with Windows changes applied within about 40 ms and without hiding a Windows write that follows a guest write, erofs images with capabilities and labels read correctly, and replaced files keep their metadata even across restarts; the first (cold) walk is still about 4x native.
