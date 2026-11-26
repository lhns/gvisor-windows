//go:build windows

package winhost

import (
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/sys/windows"
)

const maxMetaSize = 16 << 20

// openMetaStream opens the metadata stream of the file h. disposition is
// FILE_OPEN to read, FILE_OVERWRITE_IF to (re)write.
func openMetaStream(h windows.Handle, access, disposition uint32) (windows.Handle, error) {
	s, err := ntOpen(h, ":"+MetaStream, access, shareAll, disposition, 0, 0)
	if err == nil || err == unix.ENOENT {
		return s, err
	}
	// Some filesystems do not take stream names relative to a file handle:
	// open by path.
	p, perr := handlePath(h)
	if perr != nil {
		return 0, err
	}
	return ntOpen(0, p+":"+MetaStream, access, shareAll, disposition, 0, 0)
}

// readMeta returns the file's metadata stream, nil if there is none or it is
// unreadable (the file is then treated as having the mount's defaults).
func (fs *FS) readMeta(h windows.Handle) *Metadata {
	s, err := openMetaStream(h, fileReadData|fileReadAttributes, windows.FILE_OPEN)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(s)
	var si fileStandardInfo
	if err := getInfo(s, classStandard, unsafe.Pointer(&si), uint32(unsafe.Sizeof(si))); err != nil || si.EndOfFile <= 0 || si.EndOfFile > maxMetaSize {
		return nil
	}
	buf := make([]byte, si.EndOfFile)
	var n uint32
	if err := windows.ReadFile(s, buf, &n, nil); err != nil {
		return nil
	}
	m, err := DecodeMetadata(buf[:n])
	if err != nil {
		return nil
	}
	return m
}

// writeMeta stores m in the file's metadata stream. The file's modification
// time is kept: h needs FILE_WRITE_ATTRIBUTES.
func (fs *FS) writeMeta(h windows.Handle, m *Metadata) error {
	var before fileBasicInfo
	haveTime := getInfo(h, classBasic, unsafe.Pointer(&before), uint32(unsafe.Sizeof(before))) == nil
	s, err := openMetaStream(h, fileWriteData|fileReadAttributes|fileWriteAttributes, windows.FILE_OVERWRITE_IF)
	if err != nil {
		return err
	}
	data := m.Encode()
	var n uint32
	err = windows.WriteFile(s, data, &n, nil)
	windows.CloseHandle(s)
	if err != nil {
		return toErrno(err)
	}
	if haveTime {
		restore := fileBasicInfo{LastWriteTime: before.LastWriteTime}
		_ = setInfo(h, classBasic, unsafe.Pointer(&restore), uint32(unsafe.Sizeof(restore)))
	}
	return nil
}

// defaultMeta is the metadata of a file that has no stream.
func (fs *FS) defaultMeta(h windows.Handle) (*Metadata, error) {
	var bi fileBasicInfo
	if err := getInfo(h, classBasic, unsafe.Pointer(&bi), uint32(unsafe.Sizeof(bi))); err != nil {
		return nil, err
	}
	m := &Metadata{UID: fs.opts.UID, GID: fs.opts.GID}
	switch {
	case bi.FileAttributes&attrReparse != 0 && fs.isNativeLink(h):
		m.Mode = SIFLNK | 0o777
	case bi.FileAttributes&attrDirectory != 0:
		m.Mode = SIFDIR | fs.opts.DirMode
	default:
		m.Mode = SIFREG | fs.opts.FileMode
		if bi.FileAttributes&attrReadonly != 0 {
			m.Mode &^= 0o222
		}
	}
	return m, nil
}

// updateMeta reads the metadata (or the defaults), applies fn and stores the
// result. fs.metaMu must be held.
func (fs *FS) updateMeta(h windows.Handle, fn func(*Metadata) error) error {
	m := fs.readMeta(h)
	if m == nil {
		var err error
		if m, err = fs.defaultMeta(h); err != nil {
			return err
		}
	}
	if err := fn(m); err != nil {
		return err
	}
	return fs.writeMeta(h, m)
}

const xattrAccess = fileReadAttributes | fileWriteAttributes | fileReadEA | fileWriteEA

// Linux limits.
const (
	xattrNameMax = 255
	xattrSizeMax = 65536
	xattrCreate  = 1
	xattrReplace = 2
)

// GetXattr returns an extended attribute.
func (fs *FS) GetXattr(id ID, name string) ([]byte, error) {
	h, err := fs.openID(id, xattrAccess)
	if err != nil {
		return nil, err
	}
	defer fs.closeID(id, h)
	m := fs.readMeta(h)
	if m == nil {
		return nil, unix.ENODATA
	}
	v, ok := m.Xattrs[name]
	if !ok {
		return nil, unix.ENODATA
	}
	return append([]byte(nil), v...), nil
}

// SetXattr sets an extended attribute. flags is XATTR_CREATE or XATTR_REPLACE.
func (fs *FS) SetXattr(id ID, name string, value []byte, flags uint32) error {
	defer fs.noteSelf(id)
	if len(name) > xattrNameMax || name == "" {
		return unix.ERANGE
	}
	if len(value) > xattrSizeMax {
		return unix.E2BIG
	}
	h, err := fs.openID(id, xattrAccess)
	if err != nil {
		return err
	}
	defer fs.closeID(id, h)
	fs.metaMu.Lock()
	defer fs.metaMu.Unlock()
	return fs.updateMeta(h, func(m *Metadata) error {
		_, exists := m.Xattrs[name]
		if flags&xattrCreate != 0 && exists {
			return unix.EEXIST
		}
		if flags&xattrReplace != 0 && !exists {
			return unix.ENODATA
		}
		if m.Xattrs == nil {
			m.Xattrs = map[string][]byte{}
		}
		m.Xattrs[name] = append([]byte(nil), value...)
		return nil
	})
}

// ListXattr returns the names of the extended attributes, sorted.
func (fs *FS) ListXattr(id ID) ([]string, error) {
	h, err := fs.openID(id, xattrAccess)
	if err != nil {
		return nil, err
	}
	defer fs.closeID(id, h)
	m := fs.readMeta(h)
	if m == nil {
		return nil, nil
	}
	names := make([]string, 0, len(m.Xattrs))
	for n := range m.Xattrs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// RemoveXattr deletes an extended attribute.
func (fs *FS) RemoveXattr(id ID, name string) error {
	defer fs.noteSelf(id)
	h, err := fs.openID(id, xattrAccess)
	if err != nil {
		return err
	}
	defer fs.closeID(id, h)
	fs.metaMu.Lock()
	defer fs.metaMu.Unlock()
	return fs.updateMeta(h, func(m *Metadata) error {
		if _, ok := m.Xattrs[name]; !ok {
			return unix.ENODATA
		}
		delete(m.Xattrs, name)
		return nil
	})
}
