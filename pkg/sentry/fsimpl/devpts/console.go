// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

package devpts

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// gvisor-windows: the helpers below let the sandbox launcher run a container on
// a pty without a task: opening the replica through the VFS needs one
// (Terminal.OpenTTY), and the window size is set by an ioctl with a task.

// MasterIndex returns the index of the pty whose master is fd, or false if fd
// is not a devpts master.
func MasterIndex(fd *vfs.FileDescription) (uint32, bool) {
	m, ok := fd.Impl().(*masterFileDescription)
	if !ok {
		return 0, false
	}
	return m.t.n, true
}

// OpenReplicaWithoutTask opens the replica of the pty whose master is master,
// as the node replica (vfs.GetDentryAt of /dev/pts/N), without a task and
// without making it any process's controlling terminal. It also returns the
// terminal, for kernel.CreateProcessArgs.TTY.
func OpenReplicaWithoutTask(ctx context.Context, master *vfs.FileDescription, replica vfs.VirtualDentry, flags uint32) (*vfs.FileDescription, *kernel.TTY, error) {
	m, ok := master.Impl().(*masterFileDescription)
	if !ok {
		return nil, nil, linuxerr.EINVAL
	}
	m.t.root.mu.Lock()
	ri, ok := m.t.root.replicas[m.t.n]
	m.t.root.mu.Unlock()
	if !ok {
		return nil, nil, linuxerr.EIO
	}
	fd := &replicaFileDescription{inode: ri}
	fd.LockFD.Init(&ri.locks)
	if err := fd.vfsfd.Init(fd, flags, auth.CredentialsFromContext(ctx), replica.Mount(), replica.Dentry(), &vfs.FileDescriptionOptions{
		DenyPRead:   true,
		DenyPWrite:  true,
		SpecialFile: true,
	}); err != nil {
		return nil, nil, err
	}
	ri.t.ld.replicaOpen()
	return &fd.vfsfd, m.t.replicaKTTY, nil
}

// SetMasterWindowSize sets the window size of the pty whose master is fd, and
// signals SIGWINCH to its foreground process group if it changed.
func SetMasterWindowSize(fd *vfs.FileDescription, rows, cols uint16) {
	m, ok := fd.Impl().(*masterFileDescription)
	if !ok {
		return
	}
	l := m.t.ld
	l.sizeMu.Lock()
	defer l.sizeMu.Unlock()
	old := l.size
	l.size = linux.Winsize{Row: rows, Col: cols}
	if old != l.size {
		l.terminal.replicaKTTY.SignalForegroundProcessGroup(kernel.SignalInfoPriv(linux.SIGWINCH))
	}
}
