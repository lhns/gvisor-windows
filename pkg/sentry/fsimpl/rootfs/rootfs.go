// Package rootfs mounts a sandbox root filesystem: a read-only erofs image
// with gVisor's overlay on top and a memory-backed upper layer (ADR 0015).
// It is the same layering runsc/boot builds for a container root.
package rootfs

import (
	"fmt"
	"strconv"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/erofs"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/overlay"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// Register registers the filesystem types the root needs.
func Register(vfsObj *vfs.VirtualFilesystem) {
	vfsObj.MustRegisterFilesystemType(erofs.Name, &erofs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	vfsObj.MustRegisterFilesystemType(tmpfs.Name, &tmpfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{AllowUserMount: true})
	vfsObj.MustRegisterFilesystemType(overlay.Name, &overlay.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
}

// NewMountNamespace returns a mount namespace whose root is the erofs image
// behind imageFD (a file descriptor on Linux, a file HANDLE on Windows; the
// filesystem takes ownership) under a writable memory overlay.
func NewMountNamespace(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, imageFD int) (*vfs.MountNamespace, error) {
	lowerOpts := &vfs.MountOptions{
		ReadOnly: true,
		GetFilesystemOptions: vfs.GetFilesystemOptions{
			InternalMount: true,
			Data:          "ifd=" + strconv.Itoa(imageFD),
		},
	}
	lower, err := vfsObj.MountDisconnected(ctx, creds, "", erofs.Name, lowerOpts)
	if err != nil {
		return nil, fmt.Errorf("mounting the erofs image: %w", err)
	}
	defer lower.DecRef(ctx)
	lowerRoot := vfs.MakeVirtualDentry(lower, lower.Root())
	stat, err := vfsObj.StatAt(ctx, creds, &vfs.PathOperation{Root: lowerRoot, Start: lowerRoot},
		&vfs.StatOptions{Mask: linux.STATX_UID | linux.STATX_GID | linux.STATX_MODE | linux.STATX_TYPE})
	if err != nil {
		return nil, fmt.Errorf("stat of the image root: %w", err)
	}

	upper, err := vfsObj.MountDisconnected(ctx, creds, "", tmpfs.Name, &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{
			InternalMount: true,
			InternalData: tmpfs.FilesystemOpts{
				RootFileType:            uint16(stat.Mode & linux.S_IFMT),
				DisableDefaultSizeLimit: true,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating the upper layer: %w", err)
	}
	defer upper.DecRef(ctx)
	upperRoot := vfs.MakeVirtualDentry(upper, upper.Root())
	if err := vfsObj.SetStatAt(ctx, creds, &vfs.PathOperation{Root: upperRoot, Start: upperRoot}, &vfs.SetStatOptions{
		Stat: linux.Statx{
			Mask: (linux.STATX_UID | linux.STATX_GID | linux.STATX_MODE) & stat.Mask,
			UID:  stat.UID, GID: stat.GID, Mode: stat.Mode,
		},
	}); err != nil {
		return nil, err
	}

	return vfsObj.NewMountNamespace(ctx, creds, "", overlay.Name, &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{
			InternalMount: true,
			InternalData: overlay.FilesystemOptions{
				UpperRoot:  upperRoot,
				LowerRoots: []vfs.VirtualDentry{lowerRoot},
			},
		},
	}, nil)
}
