// gvisor-windows: host-folder backend of the gofer filesystem, see
// docs/adr/0005 and 0007 and docs/results/2-windows-directfs.md.

//go:build windows

package gofer

import (
	"fmt"
	"io"
	"sync/atomic"

	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/gofer/winhost"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/kernel/pipe"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/uniqueid"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// winInode is an inode of a Windows folder. It holds no host handle: every
// operation reaches the file by its NTFS file ID, so renames and deletes from
// Windows neither fail nor are blocked.
type winInode struct {
	inode

	host *winhost.FS
	id   winhost.ID
	hh   *winHandle

	// readOK and writeOK say that a handle was requested (there is nothing
	// to hold: handles are short-lived).
	readOK, writeOK atomic.Bool

	// fifo is the shared pipe of an emulated FIFO (guarded by fifoMu).
	fifo *sharedFifo

	// pinned is set while the host layer keeps the file reachable after the
	// guest unlinked it.
	pinned atomic.Bool

	// writeFloor is the time of the last guest write (Sentry clock). NTFS
	// stamps consecutive writes with the same coarse time (and ctime), so
	// the reported mtime and ctime are never older than this, as on Linux
	// where every write updates them.
	writeFloor atomic.Int64
}

// noteWrite records a guest write at now.
func (i *winInode) noteWrite(now int64) {
	for {
		old := i.writeFloor.Load()
		if now <= old || i.writeFloor.CompareAndSwap(old, now) {
			return
		}
	}
	// The cached values are refreshed from the host on the next stat.
}

// winHandle is the hostHandle of a winInode.
type winHandle struct {
	host *winhost.FS
	id   winhost.ID
}

func (h *winHandle) ReadAt(p []byte, off int64) (int, error)  { return h.host.ReadAt(h.id, p, off) }
func (h *winHandle) WriteAt(p []byte, off int64) (int, error) { return h.host.WriteAt(h.id, p, off) }
func (h *winHandle) Sync() error                              { return h.host.Sync(h.id) }
func (h *winHandle) Allocate(mode, offset, length uint64) error {
	return h.host.Allocate(h.id, uint32(mode), offset, length)
}

func winKey(a *winhost.Attr) inoKey {
	return inoKey{ino: a.ID.Ino(), devMajor: uint32(a.ID.Vol >> 32), devMinor: uint32(a.ID.Vol)}
}

// getHostFolderFilesystem implements GetFilesystem for HostFolderOptions.
func (fstype FilesystemType) getHostFolderFilesystem(ctx context.Context, vfsObj *vfs.VirtualFilesystem, mf *pgalloc.MemoryFile, hf HostFolderOptions) (*vfs.Filesystem, *vfs.Dentry, error) {
	host, err := winhost.Open(hf.Path, winhost.Options{
		UID: hf.UID, GID: hf.GID, FileMode: hf.FileMode, DirMode: hf.DirMode,
		EmulateSymlinks: hf.EmulateSymlinks, CaseSensitive: hf.CaseSensitive,
	})
	if err != nil {
		ctx.Warningf("gofer: cannot open host folder %q: %v", hf.Path, err)
		return nil, nil, err
	}
	devMinor, err := vfsObj.GetAnonBlockDevMinor()
	if err != nil {
		host.Close()
		return nil, nil, err
	}
	var fsopts filesystemOptions
	fsopts.aname = "/"
	// Every use is revalidated unless the mount asks to trust the watch
	// (write-through, so Windows readers see guest writes at once) or to be
	// the folder's only user (exclusive).
	fsopts.interop = InteropModeShared
	switch {
	case hf.Exclusive:
		fsopts.interop = InteropModeExclusive
	case hf.Watched && !hf.NoWatch:
		fsopts.interop = InteropModeWritethrough
	}
	fsopts.dcache = hostFolderMaxCachedDentries
	fsopts.dfltuid = auth.KUID(hf.UID)
	fsopts.dfltgid = auth.KGID(hf.GID)
	fsopts.forcePageCache = true
	fsopts.disableFifoOpen = true
	fsopts.emulateFifoOpen = true // a Windows folder cannot hold a FIFO; the pipe lives in the Sentry
	fsopts.charDevicePolicy = charDevEmulatedOnly
	if hf.CaseSensitive {
		if err := host.CaseSensitiveError(); err != nil {
			ctx.Warningf("gofer: host folder %q is not case sensitive: %v (the folder must be empty; names that differ only in case will collide)", hf.Path, err)
		} else {
			ctx.Infof("gofer: host folder %q is case sensitive", hf.Path)
		}
	}
	fs := &filesystem{
		mf:                mf,
		opts:              fsopts,
		clock:             ktime.RealtimeClockFromContext(ctx),
		devMinor:          devMinor,
		inoByKey:          make(map[inoKey]uint64),
		inodeByKey:        make(map[inoKey]*inode),
		devMinorByHostDev: make(map[uint64]uint32),
		hostRelease:       host.Close,
	}
	if globalDentryCache != nil {
		fs.dentryCache = globalDentryCache
	} else {
		fs.dentryCache = &dentryCache{maxCachedDentries: fsopts.dcache}
	}
	fs.vfsfs.Init(vfsObj, &fstype, fs)

	a, err := host.Stat(host.Root())
	if err == nil {
		fs.root, err = fs.newWinDentry(host, &a)
	}
	if err != nil {
		fs.vfsfs.DecRef(ctx)
		return nil, nil, err
	}
	fs.root.refs = atomicbitops.FromInt64(2)
	if !hf.NoWatch && !testNoWatcher {
		if err := host.Watch(func(ch winhost.Change) { fs.hostChange(ctx, ch) }); err == nil {
			go func() {
				<-host.WatchDone()
				fs.watchEnded(ctx)
			}()
		} else {
			if fs.opts.interop == InteropModeWritethrough {
				fs.opts.interop = InteropModeShared
			}
			ctx.Warningf("gofer: cannot watch host folder %q, changes made from Windows are noticed only when looked at: %v", hf.Path, err)
		}
	}
	return &fs.vfsfs, &fs.root.vfsd, nil
}

// hostChange applies a change made to the folder from outside: it drops or
// refreshes what is cached and notifies the guest's inotify watchers.
func (fs *filesystem) hostChange(ctx context.Context, ch winhost.Change) {
	if fs.released.Load() != 0 {
		return
	}
	if ch.Overflow {
		fs.dropAllCaches(ctx)
		return
	}
	var ds *[]*dentry
	fs.renameMu.RLock()
	defer fs.renameMuRUnlockAndCheckCaching(ctx, &ds)

	comps := strings.Split(ch.Path, "/")
	parent := fs.root
	for _, name := range comps[:len(comps)-1] {
		parent.childrenMu.Lock()
		next := parent.children[name]
		parent.childrenMu.Unlock()
		if next == nil || !next.isDir() {
			return // nothing cached below here, so nobody can watch it
		}
		parent = next
	}
	leaf := comps[len(comps)-1]
	parent.childrenMu.Lock()
	child := parent.children[leaf]
	if ch.Action != winhost.Modified {
		parent.clearDirentsLocked()
		if child == nil {
			delete(parent.children, leaf) // a cached "does not exist"
		}
	}
	parent.childrenMu.Unlock()

	var mask uint32
	switch ch.Action {
	case winhost.Added:
		mask = linux.IN_CREATE
	case winhost.Removed:
		mask = linux.IN_DELETE
	case winhost.Modified:
		mask = linux.IN_MODIFY
	case winhost.RenamedFrom:
		mask = linux.IN_MOVED_FROM
		fs.hostCookie.Store(uniqueid.InotifyCookie(ctx))
	case winhost.RenamedTo:
		mask = linux.IN_MOVED_TO
	}
	cookie := uint32(0)
	if mask == linux.IN_MOVED_FROM || mask == linux.IN_MOVED_TO {
		cookie = fs.hostCookie.Load()
	}
	if child != nil {
		child.InotifyWithParent(ctx, mask, cookie, vfs.PathEvent)
		if ch.Action == winhost.Modified {
			child.inode.updateMetadata(ctx)
			if fs.opts.interop != InteropModeShared && child.inode.isRegularFile() {
				// Cached file data is stale. Unflushed guest writes win.
				in := child.inode
				in.dataMu.Lock()
				if in.dirty.IsEmpty() {
					in.cache.DropAll(fs.mf)
				}
				in.dataMu.Unlock()
			}
		} else {
			child.invalidate(ctx, fs.vfsfs.VirtualFilesystem(), &ds)
		}
	} else {
		parent.inode.watches.Notify(ctx, leaf, mask, cookie, vfs.PathEvent, false)
	}
}

// newWinDentry creates a dentry for the file described by a, or reuses the
// cached inode of the same file ID. The dentry has no references yet.
func (fs *filesystem) newWinDentry(host *winhost.FS, a *winhost.Attr) (*dentry, error) {
	fileType := a.Mode & linux.S_IFMT
	isDir := fileType == linux.S_IFDIR
	key := winKey(a)
	devMinor, err := fs.devMinorFromKey(key)
	if err != nil {
		return nil, err
	}
	temp := struct {
		d dentry
		i winInode
	}{}
	var cacheHit bool
	temp.d.inode = fs.getOrCreateInode(key, fileType, isDir, /* dontCache */
		func() { cacheHit = true },
		func() *inode {
			temp.i.host = host
			temp.i.id = a.ID
			temp.i.hh = &winHandle{host: host, id: a.ID}
			i := &temp.i.inode
			i.fs = fs
			i.inoKey = key
			i.ino = fs.inoFromKey(key)
			i.devMinor = devMinor
			i.rdevMajor = a.RdevMajor
			i.rdevMinor = a.RdevMinor
			i.mode = atomicbitops.FromUint32(a.Mode)
			i.uid = atomicbitops.FromUint32(a.UID)
			i.gid = atomicbitops.FromUint32(a.GID)
			i.blockSize = atomicbitops.FromUint32(uint32(hostarch.PageSize))
			i.readFD = atomicbitops.FromInt32(-1)
			i.writeFD = atomicbitops.FromInt32(-1)
			i.mmapFD = atomicbitops.FromInt32(-1)
			i.size = atomicbitops.FromUint64(a.Size)
			i.atime = atomicbitops.FromInt64(a.Atime)
			i.mtime = atomicbitops.FromInt64(a.Mtime)
			i.ctime = atomicbitops.FromInt64(a.Ctime)
			i.btime = atomicbitops.FromInt64(a.Btime)
			i.btimeValid = atomicbitops.FromBool(true)
			i.nlink = atomicbitops.FromUint32(a.Nlink)
			i.init(&temp.i)
			return i
		})
	if cacheHit && !temp.d.inode.cachedMetadataAuthoritative() {
		wi := temp.d.inode.impl.(*winInode)
		wi.metadataMu.Lock()
		wi.updateFromAttrLocked(a)
		wi.metadataMu.Unlock()
	}
	temp.d.init()
	if temp.d.inode.endpoint != nil {
		temp.d.IncRef()
	}
	fs.syncMu.Lock()
	fs.syncableDentries.PushBack(&temp.d.syncableListEntry)
	fs.syncMu.Unlock()
	return &temp.d, nil
}

// updateFromAttrLocked refreshes the cached metadata.
// +checklocks:i.inode.metadataMu
func (i *winInode) updateFromAttrLocked(a *winhost.Attr) {
	i.mode.Store(i.fileType() | a.Mode&^linux.S_IFMT)
	i.uid.Store(a.UID)
	i.gid.Store(a.GID)
	if i.atimeDirty.Load() == 0 {
		i.atime.Store(a.Atime)
	}
	floor := i.writeFloor.Load()
	if i.mtimeDirty.Load() == 0 {
		i.mtime.Store(max(a.Mtime, floor))
	}
	i.ctime.Store(max(a.Ctime, floor))
	i.btime.Store(a.Btime)
	i.btimeValid.Store(true)
	i.nlink.Store(a.Nlink)
	i.updateSizeLocked(a.Size)
}

func (i *winInode) hostReadHandleOK() bool  { return i.readOK.Load() }
func (i *winInode) hostWriteHandleOK() bool { return i.writeOK.Load() }
func (i *winInode) hostReadHandle() handle {
	if !i.readOK.Load() {
		return noHandle
	}
	return handle{fd: -1, host: i.hh}
}
func (i *winInode) hostWriteHandle() handle {
	if !i.writeOK.Load() {
		return noHandle
	}
	return handle{fd: -1, host: i.hh}
}

func (i *winInode) hostOpenHandle(flags uint32) (handle, error) {
	switch i.fileType() {
	case linux.S_IFREG:
		if flags&unix.O_TRUNC != 0 {
			if err := i.host.Truncate(i.id, 0); err != nil {
				return noHandle, err
			}
		}
	case linux.S_IFDIR:
	default:
		// Emulated FIFOs, sockets and devices have nothing to open (open
		// problem: FIFOs could be served by synthetic pipes).
		return noHandle, unix.ENXIO
	}
	return handle{fd: -1, host: i.hh}, nil
}

func (i *winInode) hostUpdateHandles(readable, writable bool) {
	if readable {
		i.readOK.Store(true)
	}
	if writable {
		i.writeOK.Store(true)
	}
}

// +checklocks:i.inode.metadataMu
func (i *winInode) hostUpdateMetadataLocked() error {
	a, err := i.host.Stat(i.id)
	if err != nil {
		return err
	}
	i.updateFromAttrLocked(&a)
	return nil
}

func (i *winInode) hostChmod(mode uint16) error {
	if i.isSymlink() {
		return unix.EOPNOTSUPP
	}
	return i.host.SetAttr(i.id, winhost.SetAttr{HasMode: true, Mode: uint32(mode)})
}

func statxNs(t linux.StatxTimestamp, now func() int64) int64 {
	if t.Nsec == linux.UTIME_NOW {
		return now()
	}
	return t.ToNsec()
}

func (i *winInode) hostSetStatLocked(stat *linux.Statx) (failureMask uint32, failureErr error) {
	s := winhost.SetAttr{}
	if stat.Mask&linux.STATX_MODE != 0 {
		s.HasMode, s.Mode = true, uint32(stat.Mode&^linux.S_IFMT)
	}
	if stat.Mask&linux.STATX_UID != 0 {
		s.HasUID, s.UID = true, stat.UID
	}
	if stat.Mask&linux.STATX_GID != 0 {
		s.HasGID, s.GID = true, stat.GID
	}
	if stat.Mask&linux.STATX_SIZE != 0 {
		s.HasSize, s.Size = true, stat.Size
	}
	now := func() int64 { return i.fs.clock.Now().Nanoseconds() }
	if stat.Mask&linux.STATX_ATIME != 0 {
		s.HasAtime, s.Atime = true, statxNs(stat.Atime, now)
	}
	if stat.Mask&linux.STATX_MTIME != 0 {
		s.HasMtime, s.Mtime = true, statxNs(stat.Mtime, now)
	}
	if err := i.host.SetAttr(i.id, s); err != nil {
		return stat.Mask & (linux.STATX_MODE | linux.STATX_UID | linux.STATX_GID | linux.STATX_SIZE | linux.STATX_ATIME | linux.STATX_MTIME), err
	}
	return 0, nil
}

// The in-Sentry pipe of an emulated FIFO is kept per host file for the whole
// process, not per mount, and lives while some inode of the file does (as a
// kernel FIFO keeps its buffer while it is open).
var (
	fifoMu sync.Mutex
	fifos  = map[winhost.ID]*sharedFifo{}
)

type sharedFifo struct {
	p    *pipe.VFSPipe
	refs int
}

func (i *winInode) hostFifoPipe() *pipe.VFSPipe {
	fifoMu.Lock()
	defer fifoMu.Unlock()
	if i.fifo == nil {
		e := fifos[i.id]
		if e == nil {
			e = &sharedFifo{p: pipe.NewVFSPipe(true /* isNamed */, pipe.DefaultPipeSize)}
			fifos[i.id] = e
		}
		e.refs++
		i.fifo = e
	}
	return i.fifo.p
}

func (i *winInode) hostDestroy() {
	fifoMu.Lock()
	if e := i.fifo; e != nil {
		i.fifo = nil
		if e.refs--; e.refs <= 0 {
			delete(fifos, i.id)
		}
	}
	fifoMu.Unlock()
	if i.pinned.Swap(false) {
		i.host.Unpin(i.id)
	}
}

func (i *winInode) hostGetChild(name string) (*dentry, error) {
	a, err := i.host.Lookup(i.id, name)
	if err != nil {
		return nil, err
	}
	return i.fs.newWinDentry(i.host, &a)
}

func (i *winInode) hostListXattr(size uint64) ([]string, error) { return i.host.ListXattr(i.id) }

func (i *winInode) hostGetXattr(name string, size uint64) (string, error) {
	v, err := i.host.GetXattr(i.id, name)
	return string(v), err
}

func (i *winInode) hostSetXattr(opts *vfs.SetXattrOptions) error {
	return i.host.SetXattr(i.id, opts.Name, []byte(opts.Value), opts.Flags)
}

func (i *winInode) hostRemoveXattr(name string) error { return i.host.RemoveXattr(i.id, name) }

// created builds the dentry of a file the host layer just created.
func (i *winInode) created(a winhost.Attr, createDentry bool) (*dentry, error) {
	if !createDentry {
		return nil, nil
	}
	return i.fs.newWinDentry(i.host, &a)
}

func (i *winInode) hostMknod(name string, creds *auth.Credentials, opts *vfs.MknodOptions) (*dentry, error) {
	if opts.Endpoint != nil {
		// bind(2): a Windows folder cannot hold a socket the host could connect to, so
		// the generic MknodAt falls back to a synthetic dentry that keeps the endpoint in
		// the Sentry (the socket file exists in the guest's view of the folder only, like
		// a socket on tmpfs, and is gone with the dentry or the sandbox). Without this the
		// socket was created as metadata only and every connect was refused.
		return nil, linuxerr.EPERM
	}
	switch opts.Mode.FileType() {
	case linux.S_IFREG, linux.S_IFCHR, linux.S_IFBLK, linux.S_IFIFO, linux.S_IFSOCK:
	default:
		return nil, unix.EPERM
	}
	a, err := i.host.Create(i.id, name, winhost.Spec{
		Mode: uint32(opts.Mode), UID: uint32(creds.FilesystemKUID), GID: uint32(creds.FilesystemKGID),
		RdevMajor: opts.DevMajor, RdevMinor: opts.DevMinor,
	})
	if err != nil {
		return nil, err
	}
	return i.created(a, true)
}

func (i *winInode) hostLink(target *dentry, name string) (*dentry, error) {
	t, ok := target.inode.impl.(*winInode)
	if !ok {
		return nil, unix.EXDEV
	}
	a, err := i.host.Link(i.id, name, t.id)
	if err != nil {
		return nil, err
	}
	return i.created(a, true)
}

func (i *winInode) hostMkdir(name string, mode linux.FileMode, uid auth.KUID, gid auth.KGID, createDentry bool) (*dentry, error) {
	a, err := i.host.Create(i.id, name, winhost.Spec{Mode: linux.S_IFDIR | uint32(mode&^linux.FileTypeMask), UID: uint32(uid), GID: uint32(gid)})
	if err != nil {
		return nil, err
	}
	return i.created(a, createDentry)
}

func (i *winInode) hostSymlink(name, target string, creds *auth.Credentials) (*dentry, error) {
	a, err := i.host.Create(i.id, name, winhost.Spec{Mode: linux.S_IFLNK | 0o777, UID: uint32(creds.FilesystemKUID), GID: uint32(creds.FilesystemKGID), Target: target})
	if err != nil {
		return nil, err
	}
	return i.created(a, true)
}

func (i *winInode) hostOpenCreate(name string, accessFlags uint32, mode linux.FileMode, uid auth.KUID, gid auth.KGID, createDentry bool) (*dentry, handle, error) {
	a, err := i.host.Create(i.id, name, winhost.Spec{Mode: linux.S_IFREG | uint32(mode&^linux.FileTypeMask), UID: uint32(uid), GID: uint32(gid)})
	if err != nil {
		return nil, noHandle, err
	}
	d, err := i.created(a, createDentry)
	if err != nil {
		return nil, noHandle, err
	}
	return d, handle{fd: -1, host: &winHandle{host: i.host, id: a.ID}}, nil
}

func (i *winInode) hostGetDirentsLocked(recordDirent func(name string, key inoKey, dType uint8)) error {
	var all []winhost.Dirent
	err := i.host.ReadDir(i.id, func(d winhost.Dirent) bool {
		all = append(all, d)
		return true
	})
	if err != nil {
		return err
	}
	for _, d := range all {
		if d.Type == 0 {
			// An emulated special file (device, fifo, socket): its type is in the metadata stream, which the
			// listing does not read. The overlay recognises whiteouts by DT_CHR, and DT_UNKNOWN made it list
			// them as files (c/storage's overlay probe: "unlinkat merged/subdir: directory not empty").
			if a, err := i.host.Lookup(i.id, d.Name); err == nil {
				d.Type = uint8(a.Mode & linux.S_IFMT >> 12)
			}
		}
		recordDirent(d.Name, inoKey{ino: d.ID.Ino(), devMajor: uint32(d.ID.Vol >> 32), devMinor: uint32(d.ID.Vol)}, d.Type)
	}
	return nil
}

func (i *winInode) hostReadlink() (string, error) { return i.host.Readlink(i.id) }

// pinIfOpen keeps the file of a if the guest has it open and is about to lose
// its last name, as Linux keeps the data of an unlinked open file.
func (i *winInode) pinIfOpen(a *winhost.Attr) {
	if a.Mode&linux.S_IFMT != linux.S_IFREG || a.Nlink > 1 {
		return
	}
	i.fs.inodeMu.Lock()
	in := i.fs.inodeByKey[winKey(a)]
	i.fs.inodeMu.Unlock()
	if in == nil {
		return
	}
	wi, ok := in.impl.(*winInode)
	if !ok || !(wi.readOK.Load() || wi.writeOK.Load()) {
		return
	}
	if wi.pinned.CompareAndSwap(false, true) {
		if err := wi.host.Pin(wi.id); err != nil {
			wi.pinned.Store(false)
		}
	}
}

func (i *winInode) hostUnlink(name string, flags uint32) error {
	if a, err := i.host.Lookup(i.id, name); err == nil {
		i.pinIfOpen(&a)
	}
	return i.host.Unlink(i.id, name, flags&linux.AT_REMOVEDIR != 0)
}

func (i *winInode) hostRename(oldName string, newParent *dentry, newName string, flags uint32) error {
	np, ok := newParent.inode.impl.(*winInode)
	if !ok {
		return unix.EXDEV
	}
	if a, err := np.host.Lookup(np.id, newName); err == nil {
		i.pinIfOpen(&a)
	}
	return i.host.Rename(i.id, oldName, np.id, newName, flags)
}

func (i *winInode) hostStatfs() (linux.Statfs, error) {
	s, err := i.host.Statfs()
	if err != nil {
		return linux.Statfs{}, err
	}
	return linux.Statfs{
		BlockSize: int64(s.BlockSize), FragmentSize: int64(s.BlockSize),
		Blocks: s.Blocks, BlocksFree: s.BlocksFree, BlocksAvailable: s.BlocksAvail,
		NameLength: uint64(s.NameLength),
	}, nil
}

func (i *winInode) hostAllocate(mode, offset, length uint64) error {
	return i.host.Allocate(i.id, uint32(mode), offset, length)
}

// hostDoRevalidation checks the cached dentries against the folder: a file
// replaced or removed from Windows invalidates its dentry.
func (i *winInode) hostDoRevalidation(ctx context.Context, vfsObj *vfs.VirtualFilesystem, state *revalidateState, ds **[]*dentry) error {
	start := state.start.inode.impl.(*winInode)
	if state.refreshStart {
		start.updateMetadata(ctx)
	}
	parent := start
	for _, d := range state.dentries {
		d.inode.metadataMu.Lock()
		a, err := parent.host.Lookup(parent.id, d.name)
		if err != nil && err != unix.ENOENT {
			d.inode.metadataMu.Unlock()
			return err
		}
		found := err == nil
		if !found || d.inode.inoKey != winKey(&a) || a.Mode&linux.S_IFMT != d.inode.fileType() {
			d.inode.metadataMu.Unlock()
			if !found && d.inode.isSynthetic() {
				return nil
			}
			d.invalidate(ctx, vfsObj, ds)
			return nil
		}
		wi := d.inode.impl.(*winInode)
		wi.updateFromAttrLocked(&a) // +checklocksforce: metadataMu is locked above.
		d.inode.metadataMu.Unlock()
		parent = wi
	}
	return nil
}

var (
	_ hostInode  = (*winInode)(nil)
	_ hostHandle = (*winHandle)(nil)
	_            = linuxerr.EINVAL
	_            = io.EOF
	_            = fmt.Sprint
)

// hostFolderMaxCachedDentries is how many unreferenced dentries a host folder
// keeps: a stat walk of a big tree should not look every file up again.
const hostFolderMaxCachedDentries = 200000

// testNoWatcher keeps tests from starting the watch, to model lost events.
var testNoWatcher bool

// testDropStat, if set, runs where dropAllCaches starts its host stats.
var testDropStat func(fs *filesystem)

// dropAllCaches forgets everything cached about the folder: dentries,
// listings, "does not exist" answers, and the metadata and clean data of
// every inode. It is what the watch's overflow or failure means: changes were
// lost, so nothing cached can be trusted.
func (fs *filesystem) dropAllCaches(ctx context.Context) {
	ctx.Warningf("gofer: host folder change stream overflowed or ended: dropping every cached dentry (mounts on it are kept)")
	fs.dropAllDentries(ctx)
	// The host stats run outside renameMu, which every walk of the mount takes
	// (a pending rename's writer lock would hold them all up behind seconds of
	// stats); updateMetadata needs only the inode's own locks.
	if testDropStat != nil {
		testDropStat(fs)
	}
	fs.root.inode.updateMetadata(ctx)
	// Inodes of regular files outlive their dentries: refresh them too.
	fs.inodeMu.Lock()
	inodes := make([]*inode, 0, len(fs.inodeByKey))
	for _, in := range fs.inodeByKey {
		inodes = append(inodes, in)
	}
	fs.inodeMu.Unlock()
	for _, in := range inodes {
		if in.isSynthetic() {
			continue
		}
		in.updateMetadata(ctx)
		if fs.opts.interop != InteropModeShared && in.isRegularFile() {
			in.dataMu.Lock()
			if in.dirty.IsEmpty() {
				in.cache.DropAll(fs.mf)
			}
			in.dataMu.Unlock()
		}
	}
}

// dropAllDentries forgets the dentries and listings of the folder (dropAllCaches).
func (fs *filesystem) dropAllDentries(ctx context.Context) {
	var ds *[]*dentry
	fs.renameMu.RLock()
	defer fs.renameMuRUnlockAndCheckCaching(ctx, &ds)
	root := fs.root
	var kids []*dentry
	root.childrenMu.Lock()
	for name, c := range root.children {
		if c == nil {
			delete(root.children, name)
		} else {
			kids = append(kids, c)
		}
	}
	root.clearDirentsLocked()
	root.childrenMu.Unlock()
	for _, c := range kids {
		c.invalidate(ctx, fs.vfsfs.VirtualFilesystem(), &ds)
	}
}

// watchEnded runs when the watch stops other than by the mount closing: the
// cache can no longer be kept right, so drop it and revalidate from now on.
func (fs *filesystem) watchEnded(ctx context.Context) {
	if fs.released.Load() != 0 {
		return
	}
	fs.dropAllCaches(ctx)
	if fs.opts.interop == InteropModeWritethrough {
		fs.opts.interop = InteropModeShared
	}
}
