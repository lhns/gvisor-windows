//go:build windows

package winhost

import (
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileAllInformation is the NT class FileAllInformation: basic, standard and
// internal (index number) information in one call.
const fileAllInformation = 18

const statusBufferOverflow = windows.NTStatus(0x80000005)

// queryAll fills bi and si and returns the NTFS index number of the file.
func queryAll(h windows.Handle, bi *fileBasicInfo, si *fileStandardInfo) (uint64, error) {
	var buf [1024]byte
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtQueryInformationFile(h, &iosb, &buf[0], uint32(len(buf)), fileAllInformation)
	if err != nil && err != statusBufferOverflow { // overflow: only the name was cut off
		return 0, toErrno(err)
	}
	// FILE_ALL_INFORMATION: Basic (40 bytes), Standard (24), Internal (8), ...
	*bi = *(*fileBasicInfo)(unsafe.Pointer(&buf[0]))
	*si = *(*fileStandardInfo)(unsafe.Pointer(&buf[40]))
	return *(*uint64)(unsafe.Pointer(&buf[64])), nil
}

// isNTFS says whether the volume of h is NTFS, where the index number is the
// file ID.
func isNTFS(h windows.Handle) bool {
	var name [16]uint16
	if err := windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		return false
	}
	return windows.UTF16ToString(name[:]) == "NTFS"
}

// A very short cache of open directory handles for lookups: a walk of a
// directory opens the directory once, not once per entry. Handles are shared
// (share delete included) and dropped after dirTTL, and at once when the
// backend removes or renames anything, so Windows is not held up.
type dirCache struct {
	mu sync.Mutex
	m  map[ID]*dirEntry
}

type dirEntry struct {
	h       windows.Handle
	refs    int
	expires time.Time
	dead    bool
}

const (
	dirTTL      = 200 * time.Millisecond
	maxDirsHeld = 32
)

// dirHandle returns a handle on a directory for lookups and a function that
// releases it.
func (fs *FS) dirHandle(id ID) (windows.Handle, func(), error) {
	if h, ok := fs.pinned(id); ok {
		return h, func() {}, nil
	}
	c := &fs.dirs
	now := time.Now()
	c.mu.Lock()
	if e, ok := c.m[id]; ok && !e.dead && now.Before(e.expires) {
		e.refs++
		c.mu.Unlock()
		return e.h, func() { c.release(e) }, nil
	}
	c.mu.Unlock()
	h, err := ntOpenByID(fs.root, id, dirAccess, shareAll, 0)
	if err != nil {
		return 0, nil, err
	}
	e := &dirEntry{h: h, refs: 1, expires: now.Add(dirTTL)}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[ID]*dirEntry{}
	}
	if len(c.m) >= maxDirsHeld {
		c.sweepLocked(now)
	}
	if old, ok := c.m[id]; ok {
		old.dead = true
		c.dropLocked(old)
	}
	c.m[id] = e
	c.mu.Unlock()
	time.AfterFunc(2*dirTTL, func() {
		c.mu.Lock()
		c.sweepLocked(time.Now())
		c.mu.Unlock()
	})
	return h, func() { c.release(e) }, nil
}

func (c *dirCache) release(e *dirEntry) {
	c.mu.Lock()
	e.refs--
	c.dropLocked(e)
	c.mu.Unlock()
}

// dropLocked closes e if it is dead or expired and unused.
func (c *dirCache) dropLocked(e *dirEntry) {
	if e.refs > 0 || e.h == 0 {
		return
	}
	if e.dead || time.Now().After(e.expires) {
		windows.CloseHandle(e.h)
		e.h = 0
	}
}

func (c *dirCache) sweepLocked(now time.Time) {
	for k, e := range c.m {
		if now.After(e.expires) || e.dead {
			e.dead = true
			delete(c.m, k)
			c.dropLocked(e)
		}
	}
	for k, e := range c.m {
		if len(c.m) < maxDirsHeld {
			break
		}
		e.dead = true
		delete(c.m, k)
		c.dropLocked(e)
	}
}

// flush closes all cached handles that are not in use and marks the others
// to close on release.
func (c *dirCache) flush() {
	c.mu.Lock()
	for k, e := range c.m {
		e.dead = true
		delete(c.m, k)
		c.dropLocked(e)
	}
	c.mu.Unlock()
}
