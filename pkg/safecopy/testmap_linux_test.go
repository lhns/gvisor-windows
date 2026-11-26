// gvisor-windows: host mapping helpers for the upstream safecopy tests.

//go:build linux

package safecopy

import (
	"testing"

	"golang.org/x/sys/unix"
)

// mapNoAccessTail returns a size-byte private mapping whose bytes from off on
// are PROT_NONE (accesses raise SIGSEGV).
func mapNoAccessTail(t *testing.T, size, off int) []byte {
	t.Helper()
	m, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANONYMOUS|unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("Mmap failed: %v", err)
	}
	t.Cleanup(func() { unix.Munmap(m) })
	if err := unix.Mprotect(m[off:], unix.PROT_NONE); err != nil {
		t.Fatalf("Mprotect failed: %v", err)
	}
	return m
}

// mapTruncated returns a size-byte mapping of a file that is only valid bytes
// long (accesses past it raise SIGBUS).
func mapTruncated(t *testing.T, size, valid int) []byte {
	t.Helper()
	fd, err := unix.MemfdCreate("safecopy_test", 0)
	if err != nil {
		t.Fatalf("MemfdCreate failed: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if err := unix.Ftruncate(fd, int64(valid)); err != nil {
		t.Fatalf("Ftruncate failed: %v", err)
	}
	m, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("Mmap failed: %v", err)
	}
	t.Cleanup(func() { unix.Munmap(m) })
	// A huge page could extend past EOF. Kernels without THP reject this
	// advice.
	if err := unix.Madvise(m, unix.MADV_NOHUGEPAGE); err != nil && err != unix.EINVAL {
		t.Fatalf("Madvise failed: %v", err)
	}
	return m
}
