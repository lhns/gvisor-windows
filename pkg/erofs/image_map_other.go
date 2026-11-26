// gvisor-windows: the per-OS part of mapping an image, split from erofs.go.

//go:build !windows

package erofs

import (
	"os"

	"golang.org/x/sys/unix"
)

func mapImage(f *os.File, size int64) ([]byte, error) {
	return unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
}

func unmapImage(b []byte) { unix.Munmap(b) }
