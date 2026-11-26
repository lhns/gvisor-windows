// gvisor-windows: host folders are served only on Windows.

//go:build !windows

package gofer

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

func (fstype FilesystemType) getHostFolderFilesystem(ctx context.Context, vfsObj *vfs.VirtualFilesystem, mf *pgalloc.MemoryFile, hf HostFolderOptions) (*vfs.Filesystem, *vfs.Dentry, error) {
	return nil, nil, linuxerr.EINVAL
}
