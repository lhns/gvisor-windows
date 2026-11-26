//go:build !windows

package rootfs

import "golang.org/x/sys/unix"

// OpenImage opens an image file read-only and returns its descriptor.
func OpenImage(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
}
