// gvisor-windows: host mapping helpers for the upstream safecopy tests.

//go:build windows

package safecopy

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mapNoAccessTail returns a size-byte private mapping whose bytes from off on
// are PAGE_NOACCESS (accesses raise an access violation, the SIGSEGV case).
func mapNoAccessTail(t *testing.T, size, off int) []byte {
	t.Helper()
	p, err := windows.VirtualAlloc(0, uintptr(size), windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		t.Fatalf("VirtualAlloc failed: %v", err)
	}
	t.Cleanup(func() { windows.VirtualFree(p, 0, windows.MEM_RELEASE) })
	m := unsafe.Slice((*byte)(unsafe.Pointer(p)), size)
	if off < size {
		var old uint32
		if err := windows.VirtualProtect(p+uintptr(off), uintptr(size-off), windows.PAGE_NOACCESS, &old); err != nil {
			t.Fatalf("VirtualProtect failed: %v", err)
		}
	}
	return m
}

// mapTruncated is not available on Windows: SIGBUS corresponds to an in-page
// I/O error, which cannot be provoked deterministically without a failing
// device, so the SIGBUS cases of the upstream tests are skipped.
func mapTruncated(t *testing.T, size, valid int) []byte {
	t.Helper()
	t.Skip("no deterministic in-page error (SIGBUS) source on Windows")
	return nil
}
