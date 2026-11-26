//go:build windows

package rootfs

import "golang.org/x/sys/windows"

// OpenImage opens an image file read-only and returns its HANDLE.
func OpenImage(path string) (int, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return -1, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return -1, err
	}
	return int(h), nil
}
