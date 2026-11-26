// gvisor-windows: moved from pgalloc_test.go; needs host mmap and files.
//go:build linux

package pgalloc

import (
	"os"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
)

// mapFileWithReadOnlyPage returns a file of three pages filled with ones,
// except that the middle page is filled with b, and a shared mapping of the
// file in which the middle page is read-only. Writes to the middle page fault,
// like writes to a page of a disk-backed file that the host cannot allocate
// disk space for.
func mapFileWithReadOnlyPage(t *testing.T, b byte) (int, []byte) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "pgalloc")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	page := int(hostarch.PageSize)
	data := make([]byte, 3*page)
	for i := range data {
		data[i] = 1
	}
	for i := page; i < 2*page; i++ {
		data[i] = b
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	m, err := unix.Mmap(int(f.Fd()), 0, 3*page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("Mmap failed: %v", err)
	}
	t.Cleanup(func() { unix.Munmap(m) })
	if err := unix.Mprotect(m[page:2*page], unix.PROT_READ); err != nil {
		t.Fatalf("Mprotect failed: %v", err)
	}
	return int(f.Fd()), m
}

func TestZeroDiskBackedSkipsZeroUnwritablePage(t *testing.T) {
	fd, m := mapFileWithReadOnlyPage(t, 0)
	// Start at the second page, so that file offsets differ from offsets in
	// the zeroed slice.
	page := int(hostarch.PageSize)
	if zeroDiskBacked(fd, uint64(page), m[page:]) {
		t.Errorf("zeroDiskBacked returned true after skipping a page")
	}
	if i := slices.IndexFunc(m[page:], func(b byte) bool { return b != 0 }); i >= 0 {
		t.Errorf("zeroDiskBacked left byte %d nonzero", page+i)
	}
}

func TestZeroDiskBackedPanicsOnNonzeroUnwritablePage(t *testing.T) {
	fd, m := mapFileWithReadOnlyPage(t, 1)
	defer func() {
		if recover() == nil {
			t.Errorf("zeroDiskBacked did not panic")
		}
	}()
	zeroDiskBacked(fd, 0, m)
}
