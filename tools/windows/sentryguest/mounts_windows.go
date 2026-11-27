// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build windows

package main

import (
	"strings"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/devices/memdev"
	"gvisor.dev/gvisor/pkg/sentry/devices/ttydev"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/dev"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/devpts"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/proc"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/sys"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// mountSystem mounts /proc, /sys, /dev and /dev/pts the way runsc does for a
// container, so guests that need them (the syscall tests, shells) find them.
func mountSystem(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry) {
	opts := &vfs.RegisterFilesystemTypeOptions{AllowUserMount: true, AllowUserList: true}
	vfsObj.MustRegisterFilesystemType(proc.Name, &proc.FilesystemType{}, opts)
	vfsObj.MustRegisterFilesystemType(sys.Name, &sys.FilesystemType{}, opts)
	vfsObj.MustRegisterFilesystemType(dev.Name, &dev.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	vfsObj.MustRegisterFilesystemType(devpts.Name, &devpts.FilesystemType{}, opts)
	must(memdev.Register(vfsObj), "registering memdev")
	must(ttydev.Register(vfsObj), "registering ttydev")

	pop := func(p string, follow bool) *vfs.PathOperation {
		return &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(p), FollowFinalSymlink: follow}
	}
	for _, m := range []struct{ dir, fs, data string }{
		{"/proc", proc.Name, ""},
		{"/sys", sys.Name, ""},
		{"/dev", dev.Name, ""},
		{"/dev/pts", devpts.Name, "newinstance,ptmxmode=0666"},
	} {
		if err := vfsObj.MkdirAt(ctx, creds, pop(m.dir, false), &vfs.MkdirOptions{Mode: 0o755}); err != nil && !strings.Contains(err.Error(), "exists") {
			must(err, "mkdir "+m.dir)
		}
		_, err := vfsObj.MountAt(ctx, creds, "", pop(m.dir, true), m.fs, &vfs.MountOptions{
			GetFilesystemOptions: vfs.GetFilesystemOptions{InternalMount: true, Data: m.data},
		})
		must(err, "mounting "+m.dir)
	}
}
