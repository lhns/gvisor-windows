// gvisor-windows: the third inode implementation, see docs/adr/0005 and 0007.

package gofer

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// HostFolderOptions, passed as vfs.GetFilesystemOptions.InternalData, makes
// the filesystem serve a folder of the host directly, with no gofer process
// and no file protocol. Only the Windows backend implements it.
type HostFolderOptions struct {
	// Path is the folder.
	Path string

	// UID and GID own files that have no stored owner (default 0:0).
	UID, GID uint32

	// FileMode and DirMode are the permission bits of files and directories
	// without a stored mode (default 0644 and 0755).
	FileMode, DirMode uint32

	// EmulateSymlinks stores all symlinks in the metadata stream, never as
	// native symlinks.
	EmulateSymlinks bool

	// CaseSensitive makes directories created through the mount case
	// sensitive (needs Windows 10 1803 or later).
	CaseSensitive bool

	// Exclusive says that nothing else changes the folder while it is
	// mounted, so cached state is never revalidated. The default is to
	// revalidate, as Windows programs edit the folder.
	Exclusive bool

	// NoWatch turns off the watch for changes made from outside. Without
	// the watch, cached state is revalidated on every use.
	NoWatch bool

	// Revalidate makes every path walk check the folder instead of trusting
	// the cache and the watch (slower: a stat of a file looks it up again).
	Revalidate bool
}

// hostInode is implemented by the inode type of the host-folder backend. It
// is the same operation set as lisafsInode and directfsInode; inode_impl.go
// dispatches to it from its type switches.
type hostInode interface {
	hostReadHandleOK() bool
	hostWriteHandleOK() bool
	hostReadHandle() handle
	hostWriteHandle() handle
	hostOpenHandle(flags uint32) (handle, error)
	hostUpdateHandles(readable, writable bool)
	hostUpdateMetadataLocked() error
	hostChmod(mode uint16) error
	hostSetStatLocked(stat *linux.Statx) (failureMask uint32, failureErr error)
	hostDestroy()
	hostGetChild(name string) (*dentry, error)
	hostListXattr(size uint64) ([]string, error)
	hostGetXattr(name string, size uint64) (string, error)
	hostSetXattr(opts *vfs.SetXattrOptions) error
	hostRemoveXattr(name string) error
	hostMknod(name string, creds *auth.Credentials, opts *vfs.MknodOptions) (*dentry, error)
	hostLink(target *dentry, name string) (*dentry, error)
	hostMkdir(name string, mode linux.FileMode, uid auth.KUID, gid auth.KGID, createDentry bool) (*dentry, error)
	hostSymlink(name, target string, creds *auth.Credentials) (*dentry, error)
	hostOpenCreate(name string, accessFlags uint32, mode linux.FileMode, uid auth.KUID, gid auth.KGID, createDentry bool) (*dentry, handle, error)
	hostGetDirentsLocked(recordDirent func(name string, key inoKey, dType uint8)) error
	hostReadlink() (string, error)
	hostUnlink(name string, flags uint32) error
	hostRename(oldName string, newParent *dentry, newName string, flags uint32) error
	hostStatfs() (linux.Statfs, error)
	hostAllocate(mode, offset, length uint64) error
	hostDoRevalidation(ctx context.Context, vfsObj *vfs.VirtualFilesystem, state *revalidateState, ds **[]*dentry) error
}

// hostHandle is the I/O side of a handle of the host-folder backend. Each
// call reaches the file through a short-lived host handle.
type hostHandle interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Allocate(mode, offset, length uint64) error
}
