// gvisor-windows: a read-only file mapping of the image (ADR 0015).

//go:build windows

package erofs

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/sys/windows"
)

// mapImage maps the image read-only. The mapping handle is kept (and returned)
// so that stub processes can map the same pages.
func mapImage(f *os.File, size int64) ([]byte, uintptr, error) {
	if size == 0 {
		return nil, 0, unix.EINVAL
	}
	m, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY, 0, 0, nil)
	if err != nil {
		return nil, 0, unix.EIO
	}
	addr, err := windows.MapViewOfFile(m, windows.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		windows.CloseHandle(m)
		return nil, 0, unix.ENOMEM
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), int(size)), uintptr(m), nil
}

func closeSection(h uintptr) {
	if h != 0 {
		windows.CloseHandle(windows.Handle(h))
	}
}

func unmapImage(b []byte) {
	if len(b) > 0 {
		windows.UnmapViewOfFile(uintptr(unsafe.Pointer(&b[0])))
	}
}
