//go:build windows

package winhost

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/sys/windows"
)

// Access rights and options used throughout; some are missing from x/sys.
const (
	fileReadData        = 0x0001
	fileWriteData       = 0x0002
	fileAppendData      = 0x0004
	fileReadEA          = 0x0008
	fileWriteEA         = 0x0010
	fileTraverse        = 0x0020
	fileDeleteChild     = 0x0040
	fileReadAttributes  = 0x0080
	fileWriteAttributes = 0x0100
	deleteAccess        = 0x00010000
	synchronize         = 0x00100000

	shareAll = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

	// Classes of Get/SetFileInformationByHandle(Ex).
	classBasic            = 0
	classStandard         = 1
	classEndOfFile        = 6
	classAttributeTag     = 9
	classIDInfo           = 18
	classIDExtdDir        = 19
	classIDExtdDirRestart = 20
	classDispositionEx    = 21
	classRenameEx         = 22

	dispositionDelete         = 0x1
	dispositionPosix          = 0x2
	dispositionIgnoreReadOnly = 0x10
	renameReplace             = 0x1
	renamePosix               = 0x2
	renameIgnoreReadOnly      = 0x40

	attrReadonly     = 0x1
	attrSystem       = 0x4
	attrDirectory    = 0x10
	attrReparse      = 0x400
	reparseTagSymlnk = 0xA000000C
)

// toErrno maps an error from the windows package (Win32 errors or NTSTATUS)
// to a Linux errno.
func toErrno(err error) error {
	if err == nil {
		return nil
	}
	var e unix.Errno
	if errors.As(err, &e) {
		return err
	}
	var st windows.NTStatus
	if errors.As(err, &st) {
		switch st {
		case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND, windows.STATUS_NO_SUCH_FILE:
			return unix.ENOENT
		case windows.STATUS_OBJECT_NAME_COLLISION:
			return unix.EEXIST
		case windows.STATUS_DIRECTORY_NOT_EMPTY:
			return unix.ENOTEMPTY
		case windows.STATUS_NOT_A_DIRECTORY:
			return unix.ENOTDIR
		case windows.STATUS_FILE_IS_A_DIRECTORY:
			return unix.EISDIR
		case windows.STATUS_ACCESS_DENIED, windows.STATUS_CANNOT_DELETE:
			return unix.EACCES
		case windows.STATUS_SHARING_VIOLATION:
			return unix.EBUSY
		case windows.STATUS_DISK_FULL:
			return unix.ENOSPC
		case windows.STATUS_OBJECT_NAME_INVALID, windows.STATUS_OBJECT_PATH_SYNTAX_BAD, windows.STATUS_INVALID_PARAMETER:
			return unix.EINVAL
		case windows.STATUS_NOT_SAME_DEVICE:
			return unix.EXDEV
		case windows.STATUS_PRIVILEGE_NOT_HELD:
			return unix.EPERM
		}
		return winErrno(st.Errno())
	}
	var se syscall.Errno
	if errors.As(err, &se) {
		return winErrno(se)
	}
	return unix.EIO
}

func winErrno(e syscall.Errno) error {
	switch e {
	case windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND, windows.ERROR_INVALID_DRIVE:
		return unix.ENOENT
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_NOACCESS:
		return unix.EACCES
	case windows.ERROR_ALREADY_EXISTS, windows.ERROR_FILE_EXISTS:
		return unix.EEXIST
	case windows.ERROR_DIR_NOT_EMPTY:
		return unix.ENOTEMPTY
	case windows.ERROR_DIRECTORY:
		return unix.ENOTDIR
	case windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION:
		return unix.EBUSY
	case windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL:
		return unix.ENOSPC
	case windows.ERROR_INVALID_NAME, windows.ERROR_BAD_PATHNAME, windows.ERROR_INVALID_PARAMETER:
		return unix.EINVAL
	case windows.ERROR_NOT_SAME_DEVICE:
		return unix.EXDEV
	case windows.ERROR_PRIVILEGE_NOT_HELD:
		return unix.EPERM
	case windows.ERROR_FILENAME_EXCED_RANGE:
		return unix.ENAMETOOLONG
	case windows.ERROR_TOO_MANY_LINKS:
		return unix.EMLINK
	case windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION:
		return unix.EOPNOTSUPP
	}
	return unix.EIO
}

// ntOpen is NtCreateFile relative to root (0 for an absolute NT path).
func ntOpen(root windows.Handle, name string, access, share, disposition, options, attrs uint32) (windows.Handle, error) {
	us, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, unix.EINVAL
	}
	return ntOpenUS(root, us, access, share, disposition, options, attrs)
}

func ntOpenUS(root windows.Handle, us *windows.NTUnicodeString, access, share, disposition, options, attrs uint32) (windows.Handle, error) {
	oa := windows.OBJECT_ATTRIBUTES{
		RootDirectory: root,
		ObjectName:    us,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtCreateFile(&h, access|synchronize, &oa, &iosb, nil, attrs, share, disposition,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if err != nil {
		return 0, toErrno(err)
	}
	return h, nil
}

// ntOpenByID opens a file by its ID, relative to any handle on the volume.
func ntOpenByID(vol windows.Handle, id ID, access, share, options uint32) (windows.Handle, error) {
	raw := id.File
	n := 8
	if id.hasHigh() {
		n = 16
	}
	us := windows.NTUnicodeString{
		Length:        uint16(n),
		MaximumLength: uint16(n),
		Buffer:        (*uint16)(unsafe.Pointer(&raw[0])),
	}
	return ntOpenUS(vol, &us, access, share, windows.FILE_OPEN, options|windows.FILE_OPEN_BY_FILE_ID|windows.FILE_OPEN_REPARSE_POINT, 0)
}

func getInfo(h windows.Handle, class uint32, p unsafe.Pointer, n uint32) error {
	return toErrno(windows.GetFileInformationByHandleEx(h, class, (*byte)(p), n))
}

func setInfo(h windows.Handle, class uint32, p unsafe.Pointer, n uint32) error {
	return toErrno(windows.SetFileInformationByHandle(h, class, (*byte)(p), n))
}

type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

type fileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  uint8
	Directory      uint8
	_              uint16
}

type fileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// ID identifies a file on a volume. It is what a guest inode is: it survives
// renames of the file from either side.
type ID struct {
	Vol  uint64
	File [16]byte
}

func (id ID) hasHigh() bool {
	for _, b := range id.File[8:] {
		if b != 0 {
			return true
		}
	}
	return false
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// Ino folds the ID into an inode number.
func (id ID) Ino() uint64 {
	return le64(id.File[:8]) ^ (le64(id.File[8:]) * 0x9E3779B97F4A7C15)
}

const filetimeToUnix = 116444736000000000

// ftToUnixNs converts a FILETIME (100ns since 1601) to Unix nanoseconds.
func ftToUnixNs(ft int64) int64 { return (ft - filetimeToUnix) * 100 }

// unixNsToFt is the inverse of ftToUnixNs.
func unixNsToFt(ns int64) int64 { return ns/100 + filetimeToUnix }

var errNoMoreFiles = errors.New("no more files")

var procNtQueryDirectoryFile = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryDirectoryFile")

// fileIDExtdDirectoryInformation is the NT class of FILE_ID_EXTD_DIR_INFO.
const fileIDExtdDirectoryInformation = 60

// queryDirectory reads a batch of FILE_ID_EXTD_DIR_INFO entries into buf. It
// returns errNoMoreFiles at the end of the directory.
func queryDirectory(h windows.Handle, buf []byte, restart bool) error {
	var iosb windows.IO_STATUS_BLOCK
	var r uintptr
	if restart {
		r = 1
	}
	st, _, _ := procNtQueryDirectoryFile.Call(uintptr(h), 0, 0, 0, uintptr(unsafe.Pointer(&iosb)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), fileIDExtdDirectoryInformation, 0, 0, r)
	switch s := windows.NTStatus(st); s {
	case windows.STATUS_SUCCESS:
		return nil
	case windows.STATUS_NO_MORE_FILES:
		return errNoMoreFiles
	default:
		return toErrno(s)
	}
}
