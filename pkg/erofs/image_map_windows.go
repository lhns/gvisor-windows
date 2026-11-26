// gvisor-windows: a read-only file mapping of the image (ADR 0015).

//go:build windows

package erofs

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/sys/windows"
)

func mapImage(f *os.File, size int64) ([]byte, error) {
	if size == 0 {
		return nil, unix.EINVAL
	}
	m, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		return nil, unix.EIO
	}
	// The view keeps the mapping object alive.
	defer windows.CloseHandle(m)
	addr, err := windows.MapViewOfFile(m, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		return nil, unix.ENOMEM
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(size)), nil
}

func unmapImage(b []byte) {
	if len(b) > 0 {
		windows.UnmapViewOfFile(uintptr(unsafe.Pointer(&b[0])))
	}
}
