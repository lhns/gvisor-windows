// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

// gvisor-windows: the memfd + mmap host layer of MemoryFile, moved here from
// pgalloc.go unchanged (see pgalloc_host_windows.go for the Windows one).

package pgalloc

import (

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// hostState is the host-specific state of a MemoryFile; none on Linux.
type hostState struct{}

// hostInit truncates the file to 0 bytes to ensure that it's empty.
func (f *MemoryFile) hostInit() error {
	if err := f.file.Truncate(0); err != nil {
		return err
	}
	if !f.opts.DisableIMAWorkAround {
		hostmm.IMAWorkAroundForMemFile(f.file.Fd())
	}
	return nil
}

// hostGrow extends the backing file to newNrChunks chunks and sets the
// mappings of the new chunks in chunks. In tests, f.file may be nil.
func (f *MemoryFile) hostGrow(oldNrChunks, newNrChunks uint64, huge bool, chunks []chunkInfo) error {
	if f.file == nil {
		return nil
	}
	oldFileSize := oldNrChunks * chunkSize
	incFileSize := (newNrChunks - oldNrChunks) * chunkSize
	// If the backing file is memory-backed, and THP is enabled, Linux will
	// align our mapping to a hugepage boundary; see
	// mm/shmem.c:shmem_get_unmapped_area().
	if err := f.file.Truncate(int64(newNrChunks * chunkSize)); err != nil {
		return err
	}
	m, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		0,
		uintptr(incFileSize),
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED,
		f.file.Fd(),
		uintptr(oldFileSize))
	if errno != 0 {
		return errno
	}
	f.madviseChunkMapping(m, uintptr(incFileSize), huge)
	for i := oldNrChunks; i < newNrChunks; i++ {
		chunks[i].mapping = m
		m += chunkSize
	}
	return nil
}

// hostPrepare makes the pages in fr usable through the mappings; nothing to
// do on Linux, where they are faulted in on first touch.
func (f *MemoryFile) hostPrepare(fr memmap.FileRange) error { return nil }

// hostCanPopulate returns whether pre-populating mappings can work.
func hostCanPopulate() bool { return true }

func (f *MemoryFile) hostDestroy() {
	f.file.Close()
	// Ensure that any attempts to use f.file.Fd() fail instead of getting a fd
	// that has possibly been reassigned.
	f.file = nil
	chunks := f.chunksLoad()
	for i := range chunks {
		chunk := &chunks[i]
		_, _, errno := unix.Syscall(unix.SYS_MUNMAP, chunk.mapping, chunkSize, 0)
		if errno != 0 {
			log.Warningf("Failed to unmap mapping %#x for MemoryFile chunk %d: %v", chunk.mapping, i, errno)
		}
		chunk.mapping = 0
	}
}

func (f *MemoryFile) hostTotalUsage() (uint64, error) {
	// Stat the underlying file to discover the underlying usage. stat(2)
	// always reports the allocated block count in units of 512 bytes. This
	// includes pages in the page cache and swapped pages.
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.file.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Blocks * 512), nil
}

func (f *MemoryFile) commitFile(fr memmap.FileRange) error {
	// "The default operation (i.e., mode is zero) of fallocate() allocates the
	// disk space within the range specified by offset and len." - fallocate(2)
	return unix.Fallocate(
		int(f.file.Fd()),
		0, // mode
		int64(fr.Start),
		int64(fr.Length()))
}

func (f *MemoryFile) decommitFile(fr memmap.FileRange) error {
	// "After a successful call, subsequent reads from this range will
	// return zeroes. The FALLOC_FL_PUNCH_HOLE flag must be ORed with
	// FALLOC_FL_KEEP_SIZE in mode ..." - fallocate(2)
	return unix.Fallocate(
		int(f.file.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE,
		int64(fr.Start),
		int64(fr.Length()))
}


// gvisor-windows: the extent state of a chunk exists only on Windows.
type hostExtents struct{}

func (c *chunkInfo) hostEnsure(fr memmap.FileRange) {}

// decommitRetained is decommitFile for pages that stay allocated.
func (f *MemoryFile) decommitRetained(fr memmap.FileRange) error {
	return f.decommitFile(fr)
}
