# 2b. Root filesystem from an erofs image, Windows changes reaching the guest, safe-save metadata, host-folder speed

Nodes: "Root filesystem from an erofs image", "Windows changes reach the guest", defect "Editor safe-saves keep Linux metadata" (ADR 0015, ADR 0005/0007). All on `windows-latest` / `ubuntu-latest`; nothing run locally.

CI: windows-rootfs https://github.com/lhns/gvisor-windows/actions/runs/45888613628, windows-fs https://github.com/lhns/gvisor-windows/actions/runs/45888613629 (both green on e29f5c9f7).

## 1. erofs image as root filesystem
- `pkg/erofs`: the image mapping is per OS (`image_map_{other,windows}.go`): Windows uses `CreateFileMapping(PAGE_READONLY)` + `MapViewOfFile(FILE_MAP_READ)`, the mapping object closed after the view. erofs.go only calls `mapImage`/`unmapImage`.
- Defect found and fixed: erofs ignored the device numbers of char/block inodes (always 0:0), and the overlay treats a 0:0 char device as a whiteout, so device nodes of an image vanished. `pkg/erofs` now decodes the rdev and `fsimpl/erofs` reports it.
- Library: `pkg/sentry/fsimpl/rootfs` (`Register`, `OpenImage`, `NewMountNamespace(ctx, vfs, creds, imageFD)`): erofs mounted read-only as lower layer, tmpfs upper, overlay on top, as `runsc/boot` does for a root. No change under `runsc/`.
- Images built in Linux CI (`tools/windows/mkrootfs.sh`, ubuntu-latest, erofs-utils 1.9.1-1 / mkfs.erofs 1.9, podman 5.7.0): `podman create/export` of the OCI image, `tar -x --numeric-owner -p` as root, `mkfs.erofs -b 4096 -T 0` (uncompressed), shipped as artifact `erofs-images` with a manifest (`stat` of every entry), symlink targets and sha256 of every 40th regular file.
  | image | source | entries | image size | mount time on windows-latest |
  |---|---|---|---|---|
  | alpine | docker.io/library/alpine:3.20 (sha256:c64c687c...) | 518 | 7 MiB | 21-37 ms (first mount of the process) |
  | podman | quay.io/podman/stable:latest (sha256:5ce2ff0e...) | 16786 | 628 MiB | 0.4 ms (the image is mapped, not read) |
  | synth | generated: all file kinds, 20000-entry dir, 67 MB file | 20019 | 67 MiB | under 1 ms |
- Checked through the VFS (test context, no kernel): every manifest entry's type, mode, uid, gid, size, rdev, nlink (0 mismatches in all three), hard links share inodes (2814 hard-linked names in the podman image), all symlink targets (2435 in podman), file contents by hash (280 files, 13 MiB, 58-70 ms), large-directory listing (20000 entries), and writes land in the overlay: new directory and file, copy-up of a lower file, unlink of a lower file (whiteout), symlink and mknod in the overlay. Walking and stat-ing all 16786 podman entries took 169-176 ms.
- Not covered: xattrs in an image (inodes with xattrs are rejected by `pkg/erofs`: `ENOTSUP`; the images here have none because `tar -x` ran without xattr restore), guest `mmap` of image files (needs the platform), compressed images (unsupported by `pkg/erofs`).

## 2. Windows changes reach the guest
`winhost.FS.Watch` runs `ReadDirectoryChangesW` (overlapped, whole subtree) on the folder and reports `Added/Removed/Modified/RenamedFrom/RenamedTo` with Linux names, or an overflow. Changes made through the backend itself are filtered (names and file IDs of the backend's own operations are remembered for 750 ms). In the gofer glue each change refreshes cached metadata (and drops clean cached file data in exclusive mode), invalidates or revalidates dentries, clears directory listings and calls the guest's inotify watchers (IN_CREATE, IN_DELETE, IN_MODIFY, IN_MOVED_FROM/TO). Test `TestVFSWindowsChangesReachGuest` (exclusive cache, so the guest view really caches): a Windows write was visible to a cached stat after 11 ms (limit 1 s), the content was current, an inotify watch on the file and on the directory fired IN_MODIFY and IN_CREATE. Host layer: Added/Modified within the first poll, same-directory rename as RenamedFrom/To; a move to another directory arrives as Removed + Added.
Limits: a directory move is not tracked as a rename for inotify (no cookie pair); events lost on overflow only evict unreferenced dentries; an open file in exclusive mode with unflushed guest writes keeps its cache (guest wins); the 750 ms own-change window hides a real Windows change to the same name in that window.

## 3. Safe-save keeps Linux metadata
Windows replacing a file by rename-over gives it a new file ID and no stream. The backend remembers the metadata it last saw per (directory, name) (in memory, 100k entries) and when a lookup finds a different regular file without a stream at that name it writes the remembered mode, owner and xattrs to it. Names the guest unlinks or renames are forgotten, so a new file under that name gets defaults. Tests: host layer (write-temp-then-rename twice, then guest unlink and Windows create) and through the VFS (chown, replace, stat). Limit: the memory is per run; a replacement while no sandbox runs, or of a file the guest never looked at since start, loses the metadata (a persistent per-folder index would fix this; not built).

## 4. Host-folder speed (no handle cache)
windows-latest runner, 20000 files of 2 KB in 400 directories, mode 0751 (so each file has a metadata stream), through the VFS:
| | untar (create+write) | stat walk | 400 listings |
|---|---|---|---|
| backend, shared (revalidating) | 1.26-1.49 ms/file | 120-200 us/stat | 55-90 ms |
| backend, exclusive | 1.0-1.3 ms/file | 61-106 us/stat | 45-70 ms |
| plain os calls (WriteFile, Lstat) | 0.59-0.74 ms/file | 23-70 us/stat | |
The untar costs about twice native; a stat walk is 2-5x native (shared mode revalidates every path component by name). In absolute terms a git-status-like walk of 20000 files takes 1.2-2.4 s through the backend. A per-inode attribute cache for shared mode (fed by the new change watcher, which now makes cache invalidation cheap) is the likely next step if that is too slow; a handle cache is not indicated, the cost is the open per operation plus the stream read, and the stat walk is the one to speed up.

Verdict: a read-only erofs image built from OCI images (alpine, podman) mounts on Windows under a memory overlay in under 40 ms with every type, mode, owner, device node, hard link and symlink correct, and host folders now pick up Windows changes within milliseconds, keep metadata across editor safe-saves, and cost about 2x native for writes and 2-5x for stat walks.
