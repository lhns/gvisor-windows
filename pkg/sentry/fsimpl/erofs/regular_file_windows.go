// gvisor-windows: lets the Windows platform map the image's pages into stub
// processes instead of copying them.

//go:build windows

package erofs

import "golang.org/x/sys/windows"

// ReadOnlySection returns the read-only file mapping of the image; offsets in it
// are offsets in the image, which is what memmap.FileRange offsets of this file
// are.
func (mf *imageMemmapFile) ReadOnlySection() windows.Handle {
	return windows.Handle(mf.image.Section())
}
