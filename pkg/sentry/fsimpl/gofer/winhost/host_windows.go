//go:build windows

package winhost

import (
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"golang.org/x/sys/windows"
)

// Options configure a mount.
type Options struct {
	// UID and GID own files that have no metadata stream (like remote-docker's
	// owner= option, default 0:0).
	UID, GID uint32
	// FileMode and DirMode are the permission bits of files and directories
	// that have no metadata stream (default 0644 and 0755).
	FileMode, DirMode uint32
	// EmulateSymlinks stores every symlink in the metadata stream instead of
	// creating NTFS symlinks (which need developer mode or elevation).
	EmulateSymlinks bool
	// CaseSensitive marks directories created through the mount (and the root,
	// if it is empty) as case sensitive.
	CaseSensitive bool
}

// FS is a mounted Windows folder.
type FS struct {
	opts   Options
	path   string // root folder, Win32 form
	root   windows.Handle
	rootID ID
	vol    uint64

	metaMu sync.Mutex // serializes read-modify-write of metadata streams

	pinMu sync.Mutex
	pins  map[ID]*pin

	watcher  *watcher
	watching atomic.Bool
	ntfs     bool // file IDs are NTFS index numbers
	caseErr  error // why the root could not be made case sensitive, if asked
	dirs     dirCache
	cbMu     sync.Mutex
	recheck  map[string]bool

	shadow shadowState // metadata kept across replacements and own-change marks
}

type pin struct {
	h      windows.Handle
	refs   int
	byName bool
}

// Attr is what stat reports.
type Attr struct {
	ID    ID
	Mode  uint32 // Linux st_mode, type included
	UID   uint32
	GID   uint32
	Nlink uint32
	Size  uint64
	// Blocks is in units of 512 bytes.
	Blocks uint64
	// Times are Unix nanoseconds. Ctime is NTFS's change time.
	Atime, Mtime, Ctime, Btime int64
	RdevMajor, RdevMinor       uint32
	// Native is the NTFS attribute word.
	Native uint32
}

// Open mounts the folder at path. The folder must exist.
func Open(path string, opts Options) (*FS, error) {
	if opts.FileMode == 0 {
		opts.FileMode = 0o644
	}
	if opts.DirMode == 0 {
		opts.DirMode = 0o755
	}
	p, err := windows.UTF16PtrFromString(`\\?\` + strings.TrimPrefix(path, `\\?\`))
	if err != nil {
		return nil, unix.EINVAL
	}
	h, err := windows.CreateFile(p, fileReadAttributes|fileReadData|fileTraverse|fileWriteAttributes|synchronize, shareAll, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, toErrno(err)
	}
	fs := &FS{opts: opts, path: path, root: h, pins: map[ID]*pin{}}
	var idi fileIDInfo
	if err := getInfo(h, classIDInfo, unsafe.Pointer(&idi), uint32(unsafe.Sizeof(idi))); err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	fs.vol = idi.VolumeSerialNumber
	fs.rootID = ID{Vol: idi.VolumeSerialNumber, File: idi.FileID}
	fs.ntfs = isNTFS(h)
	fs.loadIndex()
	fs.startIndexFlusher()
	if opts.CaseSensitive {
		// Only works on an empty directory, and needs no administrator: write
		// attributes on a folder of the user's own is enough.
		fs.caseErr = fs.SetCaseSensitive(fs.rootID, true)
		if fs.caseErr == nil {
			if on, err := fs.IsCaseSensitive(fs.rootID); err == nil && !on {
				fs.caseErr = unix.EOPNOTSUPP
			}
		}
	}
	return fs, nil
}

// Close releases the root handle and pins.
func (fs *FS) Close() {
	fs.dirs.flush()
	fs.stopWatch()
	fs.stopIndexFlusher()
	fs.pinMu.Lock()
	for _, p := range fs.pins {
		windows.CloseHandle(p.h)
	}
	fs.pins = map[ID]*pin{}
	fs.pinMu.Unlock()
	windows.CloseHandle(fs.root)
}

// CaseSensitiveError says why the mounted folder could not be made case
// sensitive although Options.CaseSensitive asked for it (nil: it is).
func (fs *FS) CaseSensitiveError() error { return fs.caseErr }

// Root is the ID of the mounted folder.
func (fs *FS) Root() ID { return fs.rootID }

// Options returns the mount options.
func (fs *FS) Options() Options { return fs.opts }

// openID opens a file by ID. The caller closes the handle with closeID.
func (fs *FS) openID(id ID, access uint32) (windows.Handle, error) {
	if h, ok := fs.pinned(id); ok {
		return h, nil
	}
	return ntOpenByID(fs.root, id, access, shareAll, 0)
}

// closeID closes a handle from openID unless it belongs to a pin.
func (fs *FS) closeID(id ID, h windows.Handle) {
	if ph, ok := fs.pinned(id); ok && ph == h {
		return
	}
	windows.CloseHandle(h)
}

func (fs *FS) pinned(id ID) (windows.Handle, bool) {
	fs.pinMu.Lock()
	defer fs.pinMu.Unlock()
	p, ok := fs.pins[id]
	if !ok {
		return 0, false
	}
	return p.h, true
}

// Pin keeps a file reachable by ID after it is unlinked or replaced, as
// Linux does for open files. Pins are counted; each Pin needs an Unpin.
// A pinned handle is opened with full sharing, so Windows can still rename or
// delete the file (the name goes away at once; POSIX delete semantics).
//
// The open runs outside pinMu, which every file operation of the mount takes; of
// two Pins that open at once, the second closes its handle.
func (fs *FS) Pin(id ID) error {
	if fs.pinMore(id) {
		return nil
	}
	h, err := ntOpenByID(fs.root, id, fileReadData|fileWriteData|fileReadAttributes|fileWriteAttributes|fileReadEA|fileWriteEA|deleteAccess, shareAll, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return err
	}
	fs.pinMu.Lock()
	if p, ok := fs.pins[id]; ok {
		p.refs++
		fs.pinMu.Unlock()
		windows.CloseHandle(h)
		return nil
	}
	fs.pins[id] = &pin{h: h, refs: 1}
	fs.pinMu.Unlock()
	return nil
}

// pinMore counts one more Pin of id if it is pinned.
func (fs *FS) pinMore(id ID) bool {
	fs.pinMu.Lock()
	defer fs.pinMu.Unlock()
	if p, ok := fs.pins[id]; ok {
		p.refs++
		return true
	}
	return false
}

// pinByName makes the pin of id hold a handle opened by name instead of by
// file ID. A handle opened by ID makes Windows refuse to replace the file by
// rename (access denied whatever the access and sharing: TestRenameReplaceProbe),
// so every c/storage rewrite of layers.json, which the guest had open, fell
// back to delete + rename (a window in which the file is missing; a stop in
// it left the store without layers). A handle opened by name allows the
// replace but blocks renaming the directories above the file, so the pin is
// switched only when the file is about to be replaced.
//
// The opens and queries run outside pinMu; a reference taken under it keeps the
// pin's handle open meanwhile. The handle by ID is closed before pinByName
// returns: the replace that follows depends on it.
func (fs *FS) pinByName(id ID) {
	fs.pinMu.Lock()
	p, ok := fs.pins[id]
	if !ok || p.byName {
		fs.pinMu.Unlock()
		return
	}
	p.refs++
	old := p.h
	fs.pinMu.Unlock()
	nh := fs.openPinByName(id, old)
	fs.pinMu.Lock()
	p.refs--
	var cl []windows.Handle
	if nh != 0 {
		if p.byName || p.refs <= 0 {
			cl = append(cl, nh) // (switched by another, or no longer pinned)
		} else {
			cl = append(cl, p.h)
			p.h, p.byName = nh, true
		}
	}
	if p.refs <= 0 && fs.pins[id] == p {
		cl = append(cl, p.h)
		delete(fs.pins, id)
	}
	fs.pinMu.Unlock()
	for _, h := range cl {
		windows.CloseHandle(h)
	}
}

// openPinByName opens the file of id, open as h, by its name; 0 if it cannot.
func (fs *FS) openPinByName(id ID, h windows.Handle) windows.Handle {
	path, err := handlePath(h)
	if err != nil {
		return 0
	}
	nh, err := ntOpen(0, path, fileReadData|fileWriteData|fileReadAttributes|fileWriteAttributes|fileReadEA|fileWriteEA|deleteAccess, shareAll, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0
	}
	if a, aerr := fs.attrOf(nh); aerr != nil || a.ID != id {
		windows.CloseHandle(nh)
		return 0
	}
	return nh
}

// Unpin releases a Pin.
func (fs *FS) Unpin(id ID) {
	fs.pinMu.Lock()
	var h windows.Handle
	if p, ok := fs.pins[id]; ok {
		p.refs--
		if p.refs <= 0 {
			h = p.h
			delete(fs.pins, id)
		}
	}
	fs.pinMu.Unlock()
	// (outside pinMu, which every file operation of the mount takes: closing a
	// file the guest wrote can take milliseconds with a filter driver)
	if h != 0 {
		windows.CloseHandle(h)
	}
}

const (
	statAccess = fileReadAttributes | fileReadEA | fileReadData
)

// attrOf builds the Attr of an open file.
func (fs *FS) attrOfMeta(h windows.Handle) (Attr, *Metadata, error) {
	var a Attr
	var bi fileBasicInfo
	var si fileStandardInfo
	var idi fileIDInfo
	if fs.ntfs {
		// One call for everything. On NTFS the file ID is the index number.
		index, err := queryAll(h, &bi, &si)
		if err != nil {
			return a, nil, err
		}
		idi.VolumeSerialNumber = fs.vol
		for k := 0; k < 8; k++ {
			idi.FileID[k] = byte(index >> (8 * k))
		}
	} else {
		if err := getInfo(h, classBasic, unsafe.Pointer(&bi), uint32(unsafe.Sizeof(bi))); err != nil {
			return a, nil, err
		}
		if err := getInfo(h, classStandard, unsafe.Pointer(&si), uint32(unsafe.Sizeof(si))); err != nil {
			return a, nil, err
		}
		if err := getInfo(h, classIDInfo, unsafe.Pointer(&idi), uint32(unsafe.Sizeof(idi))); err != nil {
			return a, nil, err
		}
	}
	a.ID = ID{Vol: idi.VolumeSerialNumber, File: idi.FileID}
	a.Native = bi.FileAttributes
	a.Nlink = si.NumberOfLinks
	if si.DeletePending != 0 && a.Nlink > 0 {
		a.Nlink--
	}
	a.Size = uint64(si.EndOfFile)
	a.Blocks = uint64(si.AllocationSize) / 512
	a.Atime = ftToUnixNs(bi.LastAccessTime)
	a.Mtime = ftToUnixNs(bi.LastWriteTime)
	a.Ctime = ftToUnixNs(bi.ChangeTime)
	a.Btime = ftToUnixNs(bi.CreationTime)

	isDir := bi.FileAttributes&attrDirectory != 0
	if bi.FileAttributes&attrReparse != 0 {
		var ti fileAttributeTagInfo
		if err := getInfo(h, classAttributeTag, unsafe.Pointer(&ti), uint32(unsafe.Sizeof(ti))); err == nil && ti.ReparseTag == reparseTagSymlnk {
			a.Mode = SIFLNK | 0o777
			a.UID, a.GID = fs.opts.UID, fs.opts.GID
			a.Size = 0
			return a, nil, nil
		}
	}
	m := fs.readMeta(h)
	switch {
	case m != nil && isDir == (m.Mode&SIFMT == SIFDIR):
		a.Mode, a.UID, a.GID = m.Mode, m.UID, m.GID
		a.RdevMajor, a.RdevMinor = m.RdevMajor, m.RdevMinor
		if m.Mode&SIFMT == SIFLNK {
			a.Size = uint64(len(m.Target))
		}
	case isDir:
		a.Mode = SIFDIR | fs.opts.DirMode
		a.UID, a.GID = fs.opts.UID, fs.opts.GID
	default:
		a.Mode = SIFREG | fs.opts.FileMode
		if bi.FileAttributes&attrReadonly != 0 {
			a.Mode &^= 0o222
		}
		a.UID, a.GID = fs.opts.UID, fs.opts.GID
	}
	if isDir && a.Nlink < 2 {
		a.Nlink = 2
	}
	return a, m, nil
}

func (fs *FS) attrOf(h windows.Handle) (Attr, error) {
	a, _, err := fs.attrOfMeta(h)
	return a, err
}

// Stat returns the attributes of a file by ID.
func (fs *FS) Stat(id ID) (Attr, error) {
	h, err := fs.openID(id, statAccess)
	if err != nil {
		return Attr{}, err
	}
	defer fs.closeID(id, h)
	return fs.attrOf(h)
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) {
		return unix.EINVAL
	}
	if len(name) > 255*3 {
		return unix.ENAMETOOLONG
	}
	return nil
}

// openChild opens name in the directory dir (an open handle).
func openChild(dir windows.Handle, name string, access, options uint32) (windows.Handle, error) {
	if err := validName(name); err != nil {
		return 0, err
	}
	return ntOpen(dir, EscapeName(name), access, shareAll, windows.FILE_OPEN, options|windows.FILE_OPEN_REPARSE_POINT, 0)
}

const dirAccess = fileReadData | fileTraverse | fileReadAttributes

// Lookup finds name in the directory dir.
func (fs *FS) Lookup(dir ID, name string) (Attr, error) {
	dh, release, err := fs.dirHandle(dir)
	if err != nil {
		return Attr{}, err
	}
	defer release()
	h, err := openChild(dh, name, statAccess, 0)
	if err != nil {
		return Attr{}, err
	}
	defer windows.CloseHandle(h)
	a, m, err := fs.attrOfMeta(h)
	if err != nil {
		return a, err
	}
	return fs.noteMeta(dir, name, a, m), nil
}

// Spec describes a file to create.
type Spec struct {
	// Mode is the Linux mode, file type included (S_IFREG if no type).
	Mode      uint32
	UID, GID  uint32
	RdevMajor uint32
	RdevMinor uint32
	// Target is the target of a symlink (Mode type S_IFLNK).
	Target string
}

// Create creates name in dir. The file does not exist yet. The result is the
// new file's attributes.
func (fs *FS) Create(dir ID, name string, spec Spec) (Attr, error) {
	h, a, err := fs.create(dir, name, spec)
	if err != nil {
		return Attr{}, err
	}
	windows.CloseHandle(h)
	return a, nil
}

// create is Create but also returns the new file's handle (opened for
// reading and writing for regular files); the caller closes it.
func (fs *FS) create(dir ID, name string, spec Spec) (windows.Handle, Attr, error) {
	fs.expect(tokAppeared, name)
	if err := validName(name); err != nil {
		return 0, Attr{}, err
	}
	dh, err := fs.openID(dir, dirAccess|fileWriteData|0x2 /*FILE_ADD_FILE*/ |0x4 /*FILE_ADD_SUBDIRECTORY*/ |fileWriteAttributes)
	if err != nil {
		return 0, Attr{}, err
	}
	defer fs.closeID(dir, dh)

	typ := spec.Mode & SIFMT
	if typ == 0 {
		typ = SIFREG
		spec.Mode |= SIFREG
	}
	esc := EscapeName(name)
	access := uint32(fileReadAttributes | fileWriteAttributes | fileReadEA | fileWriteEA | deleteAccess)
	var h windows.Handle
	native := false
	switch typ {
	case SIFDIR:
		h, err = ntOpen(dh, esc, access|fileReadData|fileTraverse, shareAll, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE, 0)
		if err == nil && fs.opts.CaseSensitive {
			_ = setCaseSensitive(h, true)
		}
	case SIFLNK:
		if fs.canNativeSymlink(spec.Target) {
			if err = fs.createNativeSymlink(dh, esc, spec.Target); err == nil {
				h, err = ntOpen(dh, esc, access, shareAll, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT, 0)
				native = true
			} else if err != unix.EPERM && err != unix.EACCES && err != unix.EOPNOTSUPP && err != unix.EEXIST {
				return 0, Attr{}, err
			} else if err == unix.EEXIST {
				return 0, Attr{}, err
			}
		}
		if !native {
			attrs := uint32(attrSystem)
			h, err = ntOpen(dh, esc, access, shareAll, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE, attrs)
		}
	case SIFREG:
		h, err = ntOpen(dh, esc, access|fileReadData|fileWriteData|fileAppendData, shareAll, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE, 0)
	case SIFIFO, SIFSOCK, SIFCHR, SIFBLK:
		h, err = ntOpen(dh, esc, access, shareAll, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE, attrSystem)
	default:
		return 0, Attr{}, unix.EINVAL
	}
	if err != nil {
		return 0, Attr{}, err
	}
	var written *Metadata
	fail := func(err error) (windows.Handle, Attr, error) {
		// Best effort: remove the half-made file.
		_ = setDisposition(h)
		windows.CloseHandle(h)
		return 0, Attr{}, err
	}
	if !native {
		def := Metadata{Mode: spec.Mode, UID: spec.UID, GID: spec.GID}
		defMode := fs.opts.FileMode
		if typ == SIFDIR {
			defMode = fs.opts.DirMode
		}
		isDefault := typ == SIFREG || typ == SIFDIR
		isDefault = isDefault && spec.Mode&0o7777 == defMode && spec.UID == fs.opts.UID && spec.GID == fs.opts.GID
		if !isDefault {
			def.RdevMajor, def.RdevMinor, def.Target = spec.RdevMajor, spec.RdevMinor, spec.Target
			if err := fs.writeMeta(h, &def); err != nil {
				return fail(err)
			}
			written = &def
		}
	}
	a, err := fs.attrOf(h)
	if err != nil {
		return fail(err)
	}
	if written != nil {
		fs.remember(dir, name, a.ID, written)
	}
	return h, a, nil
}

// canNativeSymlink says whether a symlink with this Linux target can be an
// NTFS symlink with the same meaning: relative targets of plain names only.
func (fs *FS) canNativeSymlink(target string) bool {
	if fs.opts.EmulateSymlinks || target == "" || target[0] == '/' || len(target) > 1024 {
		return false
	}
	for _, r := range target {
		if needsEscape(r) || r >= 0xF000 && r < 0xF100 {
			return false
		}
	}
	return true
}

// handlePath returns the NT path (\??\C:\...) of an open file.
func handlePath(h windows.Handle) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", toErrno(err)
		}
		if int(n) <= len(buf) {
			s := string(utf16.Decode(buf[:n]))
			return `\??\` + strings.TrimPrefix(s, `\\?\`), nil
		}
		buf = make([]uint16, n+1)
	}
}

// win32Path turns an NT path from handlePath into one for Win32 calls.
func win32Path(nt string) string { return `\\?\` + strings.TrimPrefix(nt, `\??\`) }

func (fs *FS) createNativeSymlink(dh windows.Handle, esc, target string) error {
	dp, err := handlePath(dh)
	if err != nil {
		return err
	}
	link, err := windows.UTF16PtrFromString(win32Path(dp) + `\` + esc)
	if err != nil {
		return unix.EINVAL
	}
	ntarget := strings.ReplaceAll(target, "/", `\`)
	flags := uint32(0x2) // SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE
	// The kind of link must match what the target is, if it exists now.
	if th, err := ntOpen(dh, strings.ReplaceAll(EscapeName(target), "/", `\`), fileReadAttributes, shareAll, windows.FILE_OPEN, 0, 0); err == nil {
		var bi fileBasicInfo
		if getInfo(th, classBasic, unsafe.Pointer(&bi), uint32(unsafe.Sizeof(bi))) == nil && bi.FileAttributes&attrDirectory != 0 {
			flags |= windows.SYMBOLIC_LINK_FLAG_DIRECTORY
		}
		windows.CloseHandle(th)
	}
	t16, err := windows.UTF16PtrFromString(ntarget)
	if err != nil {
		return unix.EINVAL
	}
	if err := windows.CreateSymbolicLink(link, t16, flags); err != nil {
		e := toErrno(err)
		if e == unix.EACCES || e == unix.EINVAL {
			e = unix.EPERM // no privilege or no developer mode
		}
		return e
	}
	return nil
}

// Readlink returns the target of a symlink.
func (fs *FS) Readlink(id ID) (string, error) {
	h, err := fs.openID(id, statAccess)
	if err != nil {
		return "", err
	}
	defer fs.closeID(id, h)
	var bi fileBasicInfo
	if err := getInfo(h, classBasic, unsafe.Pointer(&bi), uint32(unsafe.Sizeof(bi))); err != nil {
		return "", err
	}
	if bi.FileAttributes&attrReparse != 0 {
		if t, ok := readNativeLink(h); ok {
			return t, nil
		}
	}
	if m := fs.readMeta(h); m != nil && m.Mode&SIFMT == SIFLNK {
		return m.Target, nil
	}
	return "", unix.EINVAL
}

// readNativeLink reads an NTFS symlink's target as a Linux path.
func readNativeLink(h windows.Handle) (string, bool) {
	buf := make([]byte, 16*1024)
	var n uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buf[0], uint32(len(buf)), &n, nil); err != nil {
		return "", false
	}
	if n < 20 || *(*uint32)(unsafe.Pointer(&buf[0])) != reparseTagSymlnk {
		return "", false
	}
	// REPARSE_DATA_BUFFER: tag u32, length u16, reserved u16, then
	// SubstituteNameOffset/Length, PrintNameOffset/Length (u16 each), Flags u32.
	u16 := func(o int) int { return int(*(*uint16)(unsafe.Pointer(&buf[o]))) }
	subOff, subLen := u16(8), u16(10)
	prnOff, prnLen := u16(12), u16(14)
	flags := *(*uint32)(unsafe.Pointer(&buf[16]))
	const pathBuf = 20
	name := func(off, l int) string {
		if l == 0 || pathBuf+off+l > len(buf) {
			return ""
		}
		w := make([]uint16, l/2)
		for i := range w {
			w[i] = *(*uint16)(unsafe.Pointer(&buf[pathBuf+off+2*i]))
		}
		return string(utf16.Decode(w))
	}
	s := name(prnOff, prnLen)
	if s == "" {
		s = name(subOff, subLen)
	}
	if flags&1 == 0 { // absolute: drop the NT prefix
		s = strings.TrimPrefix(s, `\??\`)
	}
	return UnescapeName(strings.ReplaceAll(s, `\`, "/")), true
}

// Link creates name in dir as a hard link to target.
func (fs *FS) Link(dir ID, name string, target ID) (Attr, error) {
	fs.expect(tokAppeared, name)
	if err := validName(name); err != nil {
		return Attr{}, err
	}
	th, err := fs.openID(target, statAccess)
	if err != nil {
		return Attr{}, err
	}
	defer fs.closeID(target, th)
	ta, err := fs.attrOf(th)
	if err != nil {
		return Attr{}, err
	}
	if ta.Mode&SIFMT == SIFDIR {
		return Attr{}, unix.EPERM
	}
	dh, err := fs.openID(dir, dirAccess)
	if err != nil {
		return Attr{}, err
	}
	defer fs.closeID(dir, dh)
	tp, err := handlePath(th)
	if err != nil {
		return Attr{}, err
	}
	dp, err := handlePath(dh)
	if err != nil {
		return Attr{}, err
	}
	np, err := windows.UTF16PtrFromString(win32Path(dp) + `\` + EscapeName(name))
	if err != nil {
		return Attr{}, unix.EINVAL
	}
	ep, err := windows.UTF16PtrFromString(win32Path(tp))
	if err != nil {
		return Attr{}, unix.EINVAL
	}
	if err := windows.CreateHardLink(np, ep, 0); err != nil {
		return Attr{}, toErrno(err)
	}
	return fs.Lookup(dir, name)
}

// setDisposition marks an open file for deletion with POSIX semantics: the
// name disappears at once even if handles are open.
func setDisposition(h windows.Handle) error {
	// The flag that deletes read-only files too is asked for only when the
	// plain delete is refused: it needs more rights.
	flags := uint32(dispositionDelete | dispositionPosix)
	err := setInfo(h, classDispositionEx, unsafe.Pointer(&flags), 4)
	if err == unix.EACCES {
		flags |= dispositionIgnoreReadOnly
		err = setInfo(h, classDispositionEx, unsafe.Pointer(&flags), 4)
		log.Infof("winhost: unlink: the delete needed the ignore-read-only flag: %v", err)
	}
	if err == unix.EINVAL || err == unix.EOPNOTSUPP {
		log.Warningf("winhost: unlink: POSIX delete refused (%v), using delete-on-close: the name stays until every handle is closed", err)
		// Older Windows: classic delete-on-close.
		d := uint32(1)
		return setInfo(h, 4 /* FileDispositionInfo */, unsafe.Pointer(&d), 1)
	}
	return err
}

// Unlink removes name from dir. rmdir selects rmdir(2) over unlink(2).
func (fs *FS) Unlink(dir ID, name string, rmdir bool) error {
	fs.dirs.flush()
	fs.forget(dir, name)
	fs.expect(tokGone, name)
	// No FILE_DELETE_CHILD on the parent: a standard user usually has DELETE on
	// the child (inherited) but not that right on the directory, and the
	// child's DELETE is enough.
	dh, err := fs.openID(dir, dirAccess)
	if err != nil {
		return err
	}
	defer fs.closeID(dir, dh)
	opts := uint32(windows.FILE_NON_DIRECTORY_FILE)
	if rmdir {
		opts = windows.FILE_DIRECTORY_FILE
	}
	h, err := openChild(dh, name, deleteAccess|fileReadAttributes|fileWriteAttributes, opts)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return setDisposition(h)
}

// Rename flags, as in renameat2(2).
const (
	RenameNoReplace = 1
	RenameExchange  = 2
	RenameWhiteout  = 4
)

// Rename moves oldName of oldDir to newName of newDir, replacing an existing
// target as rename(2) does.
func (fs *FS) Rename(oldDir ID, oldName string, newDir ID, newName string, flags uint32) error {
	if flags&^RenameNoReplace != 0 {
		return unix.EINVAL
	}
	if err := validName(newName); err != nil {
		return err
	}
	fs.dirs.flush()
	fs.forget(oldDir, oldName)
	fs.forget(newDir, newName)
	fs.expect(tokGone, oldName)
	fs.expect(tokAppeared, newName)
	oh, err := fs.openID(oldDir, dirAccess)
	if err != nil {
		return err
	}
	defer fs.closeID(oldDir, oh)
	nh := oh
	if newDir != oldDir {
		nh, err = fs.openID(newDir, dirAccess|0x2|0x4)
		if err != nil {
			return err
		}
		defer fs.closeID(newDir, nh)
	}
	sh, err := openChild(oh, oldName, deleteAccess|fileReadAttributes|fileWriteAttributes, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(sh)
	src, err := fs.attrOf(sh)
	if err != nil {
		return err
	}
	srcDir := src.Mode&SIFMT == SIFDIR

	targetFile := false
	// Check the target for the errors rename(2) defines; Windows reports
	// access denied for all of them.
	if th, err := openChild(nh, newName, statAccess, 0); err == nil {
		ta, aerr := fs.attrOf(th)
		windows.CloseHandle(th)
		if aerr != nil {
			return aerr
		}
		if flags&RenameNoReplace != 0 {
			return unix.EEXIST
		}
		if ta.ID == src.ID {
			return nil // same file: nothing to do
		}
		dstDir := ta.Mode&SIFMT == SIFDIR
		targetFile = !dstDir
		if targetFile && flags&RenameNoReplace == 0 {
			fs.pinByName(ta.ID)
		}
		switch {
		case srcDir && !dstDir:
			return unix.ENOTDIR
		case !srcDir && dstDir:
			return unix.EISDIR
		case srcDir && dstDir:
			// Replace an empty directory: remove it first (not atomic).
			if err := fs.Unlink(newDir, newName, true); err != nil {
				return err
			}
		}
	}

	np, err := handlePath(nh)
	if err != nil {
		return err
	}
	full := np + `\` + EscapeName(newName)
	err = renameHandle(sh, full, flags&RenameNoReplace == 0)
	if err == unix.EINVAL {
		// The absolute NT path was refused (ERROR_INVALID_NAME seen for long names):
		// name it relative to the target directory instead.
		e2 := renameRelative(sh, nh, EscapeName(newName), flags&RenameNoReplace == 0)
		if e2 == nil {
			log.Infof("winhost: rename to %q (%d chars) was refused; relative to the directory: %v", full, len(full), e2)
			err = e2
		}
	}
	if targetFile && (err == unix.EACCES || err == unix.EBUSY) {
		// A target that is held open or has an unusual state may refuse to be
		// replaced: delete it first (POSIX delete works while it is held) and
		// rename into the free name. Not atomic.
		log.Infof("winhost: rename %q -> %q: replacing refused (%v), deleting the target first (not atomic)", oldName, newName, err)
		if uerr := fs.Unlink(newDir, newName, false); uerr == nil {
			err = renameHandle(sh, full, false)
		} else {
			log.Warningf("winhost: rename %q -> %q: deleting the target failed: %v", oldName, newName, uerr)
		}
	}
	return err
}

// NT information classes of a rename (FILE_INFORMATION_CLASS).
const (
	ntClassRename   = 10 // FileRenameInformation
	ntClassRenameEx = 65 // FileRenameInformationEx
)

// ntRename renames the open file h with NtSetInformationFile, which takes
// the NT path as it is (SetFileInformationByHandle converts it first and
// refuses some names with ERROR_INVALID_NAME). flags are FILE_RENAME_*; for
// the classic class only the replace bit counts. A root handle makes name
// relative to that directory.
func ntRename(h windows.Handle, class, flags uint32, root windows.Handle, name string) error {
	w := utf16.Encode([]rune(name))
	// FILE_RENAME_INFORMATION(_EX): Flags/ReplaceIfExists u32 (+pad),
	// RootDirectory HANDLE, FileNameLength u32, FileName.
	const hdr = 20
	size := hdr + len(w)*2
	buf := make([]uint64, (size+7)/8)
	b := unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), len(buf)*8)
	if class == ntClassRename {
		flags &= renameReplace
	}
	*(*uint32)(unsafe.Pointer(&b[0])) = flags
	*(*uintptr)(unsafe.Pointer(&b[8])) = uintptr(root)
	*(*uint32)(unsafe.Pointer(&b[16])) = uint32(len(w) * 2)
	for i, c := range w {
		*(*uint16)(unsafe.Pointer(&b[hdr+2*i])) = c
	}
	var iosb windows.IO_STATUS_BLOCK
	raw := windows.NtSetInformationFile(h, &iosb, &b[0], uint32(size), class)
	err := toErrno(raw)
	if err == unix.EACCES || err == unix.EBUSY || err == unix.EINVAL {
		log.Infof("winhost: rename class %d flags %#x root %v to %q: %v (%v)", class, flags, root != 0, name, raw, err)
	}
	return err
}

// renameHandle renames an open file to the NT path full.
func renameHandle(h windows.Handle, full string, replace bool) error {
	return renameWith(h, 0, full, replace)
}

// renameRelative renames h to name inside the directory handle dir.
func renameRelative(h, dir windows.Handle, name string, replace bool) error {
	return renameWith(h, dir, name, replace)
}

func renameWith(h, root windows.Handle, name string, replace bool) error {
	flags := uint32(0)
	if replace {
		flags = renameReplace | renamePosix
	}
	err := ntRename(h, ntClassRenameEx, flags, root, name)
	if err == unix.EACCES && replace {
		// Replacing a read-only file needs the flag that ignores the attribute;
		// it is not asked for first because it needs more rights.
		if err2 := ntRename(h, ntClassRenameEx, flags|renameIgnoreReadOnly, root, name); err2 != unix.EINVAL {
			err = err2
		} // else the flag is refused to this user: keep the access error
	}
	if err == unix.EACCES && replace {
		// A standard user is refused the POSIX replace of an existing file
		// (seen on every c/storage rewrite of layers.json). The plain replace
		// is still one atomic operation, unlike delete + rename.
		if err2 := ntRename(h, ntClassRenameEx, renameReplace, root, name); err2 == nil {
			log.Infof("winhost: rename to %q: the plain replace worked where the POSIX replace was refused", name)
			return nil
		} else if err3 := ntRename(h, ntClassRename, renameReplace, root, name); err3 == nil {
			log.Infof("winhost: rename to %q: the classic replace worked where the Ex replaces were refused (%v)", name, err2)
			return nil
		} else {
			log.Infof("winhost: rename to %q: every replace refused: POSIX %v, plain %v, classic %v", name, err, err2, err3)
		}
	}
	if err == unix.EINVAL || err == unix.EOPNOTSUPP {
		// No POSIX rename (before Windows 10 1809), or a refused name: classic rename.
		first := err
		err = ntRename(h, ntClassRename, flags, root, name)
		if err != nil {
			log.Infof("winhost: rename to %q (%d chars, replace %v, root %v): POSIX rename gave %v, classic rename gave %v", name, len(name), replace, root != 0, first, err)
		}
	}
	return err
}

// Dirent is one directory entry.
type Dirent struct {
	Name string
	ID   ID
	// Type is a DT_* value; 0 (DT_UNKNOWN) for files whose type is in the
	// metadata stream (the listing does not read it).
	Type uint8
}

// ReadDir lists a directory, without "." and "..".
func (fs *FS) ReadDir(dir ID, fn func(Dirent) bool) error {
	dh, err := fs.openID(dir, dirAccess)
	if err != nil {
		return err
	}
	defer fs.closeID(dir, dh)
	backing := make([]uint64, 8*1024) // 64 KB, 8-byte aligned
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&backing[0])), len(backing)*8)
	restart := true
	for {
		if err := queryDirectory(dh, buf, restart); err != nil {
			if err == errNoMoreFiles {
				return nil
			}
			return err
		}
		restart = false
		off := 0
		for {
			e := buf[off:]
			next := *(*uint32)(unsafe.Pointer(&e[0]))
			attrs := *(*uint32)(unsafe.Pointer(&e[56]))
			nameLen := int(*(*uint32)(unsafe.Pointer(&e[60])))
			tag := *(*uint32)(unsafe.Pointer(&e[68]))
			w := make([]uint16, nameLen/2)
			for i := range w {
				w[i] = *(*uint16)(unsafe.Pointer(&e[88+2*i]))
			}
			name := string(utf16.Decode(w))
			if name != "." && name != ".." {
				d := Dirent{Name: UnescapeName(name), ID: ID{Vol: fs.vol}}
				copy(d.ID.File[:], e[72:88])
				switch {
				case attrs&attrReparse != 0 && tag == reparseTagSymlnk:
					d.Type = 10 // DT_LNK
				case attrs&attrDirectory != 0:
					d.Type = 4 // DT_DIR
				case attrs&attrSystem != 0:
					d.Type = 0 // emulated special file: type is in the stream
				default:
					d.Type = 8 // DT_REG
				}
				if !fn(d) {
					return nil
				}
			}
			if next == 0 {
				break
			}
			off += int(next)
		}
	}
}

// SetAttr changes attributes. Fields are applied if their Has* flag is set.
type SetAttr struct {
	HasMode, HasUID, HasGID, HasSize, HasAtime, HasMtime bool
	// Mode holds permission bits only (the file type is kept).
	Mode     uint32
	UID, GID uint32
	Size     uint64
	// Atime and Mtime are Unix nanoseconds.
	Atime, Mtime int64
}

// SetAttr applies changes to a file.
func (fs *FS) SetAttr(id ID, s SetAttr) error {
	defer fs.noteSelf(id) // runs after the handles are closed
	access := uint32(fileReadAttributes | fileWriteAttributes | fileReadEA | fileWriteEA)
	if s.HasSize {
		access |= fileWriteData
	}
	h, err := fs.openID(id, access)
	if err != nil {
		return err
	}
	defer fs.closeID(id, h)

	if s.HasMode || s.HasUID || s.HasGID {
		fs.metaMu.Lock()
		err := fs.updateMeta(id, h, func(m *Metadata) error {
			if m.Mode&SIFMT == SIFLNK && fs.isNativeLink(h) {
				if s.HasMode {
					return unix.EOPNOTSUPP
				}
			}
			if s.HasMode {
				m.Mode = m.Mode&SIFMT | s.Mode&0o7777
			}
			if s.HasUID {
				m.UID = s.UID
			}
			if s.HasGID {
				m.GID = s.GID
			}
			return nil
		})
		fs.metaMu.Unlock()
		if err != nil {
			return err
		}
	}
	if s.HasSize {
		sz := int64(s.Size)
		if err := setInfo(h, classEndOfFile, unsafe.Pointer(&sz), 8); err != nil {
			return err
		}
	}
	if s.HasAtime || s.HasMtime {
		bi := fileBasicInfo{} // zero fields stay unchanged
		if s.HasAtime {
			bi.LastAccessTime = unixNsToFt(s.Atime)
		}
		if s.HasMtime {
			bi.LastWriteTime = unixNsToFt(s.Mtime)
		}
		if err := setInfo(h, classBasic, unsafe.Pointer(&bi), uint32(unsafe.Sizeof(bi))); err != nil {
			return err
		}
	}
	return nil
}

func (fs *FS) isNativeLink(h windows.Handle) bool {
	var bi fileBasicInfo
	if getInfo(h, classBasic, unsafe.Pointer(&bi), uint32(unsafe.Sizeof(bi))) != nil || bi.FileAttributes&attrReparse == 0 {
		return false
	}
	_, ok := readNativeLink(h)
	return ok
}

// Truncate sets the size of a regular file.
func (fs *FS) Truncate(id ID, size uint64) error {
	return fs.SetAttr(id, SetAttr{HasSize: true, Size: size})
}

// Allocate extends a file to at least off+length (fallocate mode 0).
func (fs *FS) Allocate(id ID, mode uint32, off, length uint64) error {
	defer fs.noteSelf(id) // runs after the handles are closed
	if mode != 0 {
		return unix.EOPNOTSUPP
	}
	h, err := fs.openID(id, fileReadAttributes|fileWriteData)
	if err != nil {
		return err
	}
	defer fs.closeID(id, h)
	var si fileStandardInfo
	if err := getInfo(h, classStandard, unsafe.Pointer(&si), uint32(unsafe.Sizeof(si))); err != nil {
		return err
	}
	if want := int64(off + length); want > si.EndOfFile {
		return setInfo(h, classEndOfFile, unsafe.Pointer(&want), 8)
	}
	return nil
}

// ReadAt reads from a regular file through a short-lived handle.
func (fs *FS) ReadAt(id ID, p []byte, off int64) (int, error) {
	h, err := fs.openID(id, fileReadData)
	if err != nil {
		return 0, err
	}
	defer fs.closeID(id, h)
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > 1<<30 {
			chunk = chunk[:1<<30]
		}
		var n uint32
		ov := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
		err := windows.ReadFile(h, chunk, &n, &ov)
		total += int(n)
		if err != nil {
			if err == windows.ERROR_HANDLE_EOF {
				return total, nil
			}
			return total, toErrno(err)
		}
		if n == 0 {
			break
		}
		p = p[n:]
		off += int64(n)
	}
	return total, nil
}

// WriteAt writes to a regular file through a short-lived handle.
func (fs *FS) WriteAt(id ID, p []byte, off int64) (int, error) {
	defer fs.noteSelf(id) // runs after the handles are closed
	h, err := fs.openID(id, fileWriteData)
	if err != nil {
		return 0, err
	}
	defer fs.closeID(id, h)
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > 1<<30 {
			chunk = chunk[:1<<30]
		}
		var n uint32
		ov := windows.Overlapped{Offset: uint32(off), OffsetHigh: uint32(off >> 32)}
		if err := windows.WriteFile(h, chunk, &n, &ov); err != nil {
			return total, toErrno(err)
		}
		total += int(n)
		p = p[n:]
		off += int64(n)
	}
	return total, nil
}

// Sync flushes a file to disk.
func (fs *FS) Sync(id ID) error {
	h, err := fs.openID(id, fileWriteData)
	if err != nil {
		// A read-only file has nothing to flush.
		return nil
	}
	defer fs.closeID(id, h)
	return toErrno(windows.FlushFileBuffers(h))
}

// Statfs describes the volume of the mount.
type Statfs struct {
	BlockSize                       uint32
	Blocks, BlocksFree, BlocksAvail uint64
	NameLength                      uint32
}

// Statfs reports the volume's space.
func (fs *FS) Statfs() (Statfs, error) {
	p, err := windows.UTF16PtrFromString(fs.path)
	if err != nil {
		return Statfs{}, unix.EINVAL
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return Statfs{}, toErrno(err)
	}
	const bs = 4096
	return Statfs{BlockSize: bs, Blocks: total / bs, BlocksFree: free / bs, BlocksAvail: avail / bs, NameLength: 255}, nil
}

// setCaseSensitive sets or clears NTFS per-directory case sensitivity on an
// open (empty) directory. Class 71 is FileCaseSensitiveInformation.
func setCaseSensitive(h windows.Handle, on bool) error {
	flags := uint32(0)
	if on {
		flags = 1
	}
	var iosb windows.IO_STATUS_BLOCK
	return toErrno(windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&flags)), 4, 71))
}

// IsCaseSensitive reports whether a directory is case sensitive.
func (fs *FS) IsCaseSensitive(dir ID) (bool, error) {
	h, err := fs.openID(dir, fileReadAttributes)
	if err != nil {
		return false, err
	}
	defer fs.closeID(dir, h)
	var flags uint32
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtQueryInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&flags)), 4, 71); err != nil {
		return false, toErrno(err)
	}
	return flags&1 != 0, nil
}

