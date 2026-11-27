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

package gofer

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// subtreeMounted returns true if d or a cached descendant has a mount on it.
//
// gvisor-windows: the host-folder backend invalidates dentries when its
// change stream reports a change or overflows. vfs.InvalidateDentry unmounts
// what is mounted on an invalidated dentry, and a dentry that is dropped from
// its parent is replaced by a fresh one with no mount on it, so a container's
// /dev or /tmp (tmpfs mounted on a directory of the host folder) vanished.
//
// Precondition: fs.renameMu must be locked.
func (d *dentry) subtreeMounted() bool {
	if d.vfsd.IsMounted() {
		return true
	}
	if !d.isDir() {
		return false
	}
	d.childrenMu.Lock()
	kids := make([]*dentry, 0, len(d.children))
	for _, c := range d.children {
		if c != nil {
			kids = append(kids, c)
		}
	}
	d.childrenMu.Unlock()
	for _, c := range kids {
		if c.subtreeMounted() {
			return true
		}
	}
	return false
}

// invalidateAroundMounts invalidates what is cached below d except d itself,
// which stays (with its mount or the dentries that lead to one): its listing
// and metadata are refreshed and its children are invalidated one by one,
// recursively keeping the dentries that lead to a mount.
//
// Precondition: fs.renameMu must be locked.
func (d *dentry) invalidateAroundMounts(ctx context.Context, vfsObj *vfs.VirtualFilesystem, ds **[]*dentry) {
	log.Debugf("gofer: invalidating around the mounts below %q", d.name)
	if d.isDir() {
		d.opMu.RLock()
		d.childrenMu.Lock()
		kids := make([]*dentry, 0, len(d.children))
		for _, c := range d.children {
			if c != nil {
				kids = append(kids, c)
			}
		}
		d.clearDirentsLocked()
		d.childrenMu.Unlock()
		d.opMu.RUnlock()
		for _, c := range kids {
			c.invalidate(ctx, vfsObj, ds)
		}
	}
	d.inode.updateMetadata(ctx)
}
