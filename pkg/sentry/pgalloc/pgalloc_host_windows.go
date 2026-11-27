// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

// The Windows host layer of MemoryFile. Instead of a memfd and mmap, every
// 1 GB chunk is an address range of the Sentry (a placeholder) that is filled
// extent by extent: each 2 MiB extent in use is its own pagefile-backed
// section, created when its pages are first allocated and mapped into the
// placeholder, and unmapped and closed when its pages are all freed. See
// docs/adr/0016-memory-file-on-extent-sections.md (it replaces ADR 0013).
//
//   - Windows cannot decommit a view of a section, so memory only goes back to
//     the system (working set and commit charge) when the whole section is
//     closed; sections of 2 MiB make that possible for freed ranges.
//   - Pages of a fresh section read as zero, so a released extent needs no
//     clearing.
//   - Partly freed extents are zeroed with DiscardVirtualMemory (which also
//     drops their physical pages), and checked.
package pgalloc

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

const (
	extentShift     = 21
	extentSize      = 1 << extentShift // 2 MiB
	extentMask      = extentSize - 1
	extentsPerChunk = chunkSize / extentSize

	memReserve       = 0x2000
	memRelease       = 0x8000
	memPlaceholder   = 0x40000
	memReplacePH     = 0x4000
	memPreservePH    = 0x2
	memCoalescePH    = 0x1
	pageNoAccess     = 1
	pageReadWriteWin = 4
)

var (
	modKernel32           = windows.NewLazySystemDLL("kernel32.dll")
	modKernelBase         = windows.NewLazySystemDLL("kernelbase.dll")
	procDiscardVirtualMem = modKernel32.NewProc("DiscardVirtualMemory")
	procQueryWorkingSetEx = modKernel32.NewProc("K32QueryWorkingSetEx")
	procVirtualAlloc2     = modKernelBase.NewProc("VirtualAlloc2")
	procVirtualFreeEx     = modKernel32.NewProc("VirtualFreeEx")
	procMapViewOfFile3    = modKernelBase.NewProc("MapViewOfFile3")
	procUnmapViewOfFile2  = modKernelBase.NewProc("UnmapViewOfFile2")
)

// hostExtents is the state of the extents of one chunk.
type hostExtents struct {
	mu sync.Mutex
	// sec holds the section handle of each extent, 0 if the extent has none
	// (its address range is a placeholder). Written under mu, read atomically.
	sec [extentsPerChunk]uintptr
}

// hostState is the Windows-specific state of a MemoryFile.
type hostState struct {
	// enabled is set by hostInit. A MemoryFile built without NewMemoryFile
	// (as in tests of the allocator) has no backing sections.
	enabled bool
}

// hostInit takes ownership of file, which is not used: the backing store is
// the extent sections. file may be nil.
func (f *MemoryFile) hostInit() error {
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	f.host.enabled = true
	return nil
}

// hostGrow reserves the address range of each new chunk.
func (f *MemoryFile) hostGrow(oldNrChunks, newNrChunks uint64, huge bool, chunks []chunkInfo) error {
	if !f.host.enabled {
		return nil
	}
	for i := oldNrChunks; i < newNrChunks; i++ {
		r, _, err := procVirtualAlloc2.Call(uintptr(windows.CurrentProcess()), 0, chunkSize, memReserve|memPlaceholder, pageNoAccess, 0, 0)
		if r == 0 {
			f.hostUnmapChunks(chunks[oldNrChunks:i])
			log.Warningf("Reserving address space for MemoryFile chunk %d failed: %v", i, err)
			return linuxerr.ENOMEM
		}
		chunks[i].mapping = r
		chunks[i].ext = &hostExtents{}
	}
	return nil
}

// createExtent creates and maps the section of extent e of the chunk.
// Preconditions: x.mu is held and the extent has no section.
func (x *hostExtents) createExtent(mapping uintptr, e uint64) error {
	h, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, extentSize, nil)
	if err != nil {
		return err
	}
	addr := mapping + uintptr(e)*extentSize
	// Make the extent a placeholder of its own (fails harmlessly if it is
	// one already), then replace it with a view of the section.
	procVirtualFreeEx.Call(uintptr(windows.CurrentProcess()), addr, extentSize, memRelease|memPreservePH)
	if r, _, err := procMapViewOfFile3.Call(uintptr(h), uintptr(windows.CurrentProcess()), addr, 0, extentSize, memReplacePH, pageReadWriteWin, 0, 0); r == 0 {
		windows.CloseHandle(h)
		return err
	}
	atomic.StoreUintptr(&x.sec[e], uintptr(h))
	return nil
}

// releaseExtent unmaps and closes the section of extent e, keeping its
// address range as a placeholder.
// Preconditions: x.mu is held.
func (x *hostExtents) releaseExtent(mapping uintptr, e uint64) {
	h := x.sec[e]
	if h == 0 {
		return
	}
	addr := mapping + uintptr(e)*extentSize
	if r, _, err := procUnmapViewOfFile2.Call(uintptr(windows.CurrentProcess()), addr, memPreservePH); r == 0 {
		log.Warningf("Failed to unmap extent %#x of a MemoryFile chunk: %v", addr, err)
	}
	windows.CloseHandle(windows.Handle(h))
	atomic.StoreUintptr(&x.sec[e], 0)
	extentsReleased.Add(1)
}

// ensure creates the sections of the extents that fr (a range of the chunk
// with this mapping) touches and that have none.
func (x *hostExtents) ensure(mapping uintptr, fr memmap.FileRange) error {
	for e := (fr.Start & chunkMask) >> extentShift; e <= ((fr.End-1)&chunkMask)>>extentShift; e++ {
		if atomic.LoadUintptr(&x.sec[e]) != 0 {
			continue
		}
		x.mu.Lock()
		var err error
		if x.sec[e] == 0 {
			err = x.createExtent(mapping, e)
		}
		x.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// hostEnsure makes the pages of fr accessible through the chunk's mapping: an
// extent released by Decommit comes back (zeroed) when it is used again.
func (c *chunkInfo) hostEnsure(fr memmap.FileRange) {
	if c.ext == nil {
		return
	}
	if err := c.ext.ensure(c.mapping, fr); err != nil {
		panic(fmt.Sprintf("cannot back %v of the MemoryFile with memory: %v", fr, err))
	}
}

func (f *MemoryFile) hostUnmapChunks(chunks []chunkInfo) {
	self := uintptr(windows.CurrentProcess())
	for i := range chunks {
		chunk := &chunks[i]
		if chunk.mapping == 0 {
			continue
		}
		if x := chunk.ext; x != nil {
			x.mu.Lock()
			for e := uint64(0); e < extentsPerChunk; e++ {
				x.releaseExtent(chunk.mapping, e)
			}
			x.mu.Unlock()
		}
		// Merge the placeholders back and free the range.
		procVirtualFreeEx.Call(self, chunk.mapping, chunkSize, memRelease|memCoalescePH)
		procVirtualFreeEx.Call(self, chunk.mapping, 0, memRelease)
		chunk.mapping = 0
	}
}

func (f *MemoryFile) hostDestroy() {
	f.hostUnmapChunks(f.chunksLoad())
}

// hostPrepare backs the pages in fr with sections (committing them). Newly
// created extents read as zero.
func (f *MemoryFile) hostPrepare(fr memmap.FileRange) error {
	if !f.host.enabled {
		return nil
	}
	var err error
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		if e := chunk.ext.ensure(chunk.mapping, chunkFR); e != nil {
			log.Warningf("Failed to back %v of MemoryFile: %v", chunkFR, e)
			err = linuxerr.ENOMEM
			return false
		}
		return true
	})
	return err
}

// hostCanPopulate returns false: hostPrepare commits the pages, and there is
// no pre-faulting call that is worth its cost here.
func hostCanPopulate() bool { return false }

func (f *MemoryFile) commitFile(fr memmap.FileRange) error {
	return f.hostPrepare(fr)
}

// decommitFile makes the pages in fr read as zero and returns their memory to
// the system where it can: extents that fr covers entirely are unmapped and
// their sections closed (working set and commit charge go down); other pages
// are discarded (physical memory goes down, commit charge stays). It is for
// pages nobody uses; Decommit, whose pages stay accessible, calls
// decommitRetained. It never fails.
func (f *MemoryFile) decommitFile(fr memmap.FileRange) error {
	f.decommit(fr, true)
	return nil
}

// decommitRetained is decommitFile for pages that may still be mapped by
// others (stub processes): their sections are kept.
func (f *MemoryFile) decommitRetained(fr memmap.FileRange) error {
	f.decommit(fr, false)
	return nil
}

func (f *MemoryFile) decommit(fr memmap.FileRange, release bool) {
	if !f.host.enabled {
		return
	}
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		x := chunk.ext
		x.mu.Lock()
		defer x.mu.Unlock()
		start, end := chunkFR.Start&chunkMask, (chunkFR.End-1)&chunkMask+1
		for e := start >> extentShift; e <= (end-1)>>extentShift; e++ {
			if x.sec[e] == 0 {
				continue // already released: reads as zero once it is backed again
			}
			lo, hi := max(start, e<<extentShift), min(end, (e+1)<<extentShift)
			if release && lo == e<<extentShift && hi == (e+1)<<extentShift {
				x.releaseExtent(chunk.mapping, e)
				continue
			}
			discardOrClear(chunk.mapping+uintptr(lo), uintptr(hi-lo))
		}
		return true
	})
}

// go vet "possible misuse" here and in hostResident: addr and chunk.mapping are VirtualAlloc/MapViewOfFile memory, not Go memory.
func discardOrClear(addr, length uintptr) {
	bs := unsafe.Slice((*byte)(unsafe.Pointer(addr)), length)
	r, _, _ := procDiscardVirtualMem.Call(addr, length)
	if r == 0 {
		zero := true
		for i := 0; i+8 <= len(bs); i += 8 {
			if *(*uint64)(unsafe.Pointer(&bs[i])) != 0 {
				zero = false
				break
			}
		}
		if zero {
			discardedBytes.Add(uint64(length))
			return
		}
		nonZeroAfterDiscard.Add(1)
	} else {
		lastDiscardError.Store(uint64(r))
	}
	clearedBytes.Add(uint64(length))
	clear(bs)
}

var (
	discardedBytes      atomic.Uint64
	clearedBytes        atomic.Uint64
	nonZeroAfterDiscard atomic.Uint64
	lastDiscardError    atomic.Uint64
	extentsReleased     atomic.Uint64
)

// DecommitStats returns how many bytes Decommit zeroed with
// DiscardVirtualMemory and how many by clearing them (because the call failed
// or left non-zero bytes), how often the discarded range was not zero, and
// the last error code of DiscardVirtualMemory.
func DecommitStats() (discarded, cleared, nonZero, lastErr uint64) {
	return discardedBytes.Load(), clearedBytes.Load(), nonZeroAfterDiscard.Load(), lastDiscardError.Load()
}

// ExtentsReleased returns how many extent sections have been closed.
func ExtentsReleased() uint64 { return extentsReleased.Load() }

// hostResident sets buf[i]&1 if page i of s is resident in this process's
// working set, like mincore(2).
func hostResident(s []byte, buf []byte) error {
	type wsEx struct {
		virtualAddress    uintptr
		virtualAttributes uintptr
	}
	const batch = 1 << 15
	entries := make([]wsEx, min(len(buf), batch))
	base := uintptr(unsafe.Pointer(unsafe.SliceData(s)))
	for i := 0; i < len(buf); i += batch {
		n := min(batch, len(buf)-i)
		for j := 0; j < n; j++ {
			entries[j] = wsEx{virtualAddress: base + uintptr(i+j)*uintptr(hostarch.PageSize)}
		}
		r, _, err := procQueryWorkingSetEx.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&entries[0])), uintptr(n)*unsafe.Sizeof(entries[0]))
		if r == 0 {
			return fmt.Errorf("QueryWorkingSetEx: %w", err)
		}
		for j := 0; j < n; j++ {
			buf[i+j] = byte(entries[j].virtualAttributes & 1)
		}
	}
	return nil
}

func (f *MemoryFile) hostTotalUsage() (uint64, error) {
	var total uint64
	buf := make([]byte, chunkSize/uint64(hostarch.PageSize))
	for _, chunk := range f.chunksLoad() {
		if chunk.mapping == 0 {
			continue
		}
		s := unsafe.Slice((*byte)(unsafe.Pointer(chunk.mapping)), chunkSize)
		if err := hostResident(s, buf); err != nil {
			return 0, err
		}
		for _, b := range buf {
			total += uint64(b & 1)
		}
	}
	return total * uint64(hostarch.PageSize), nil
}

// SectionRange describes the part of a section that backs a file range, for
// mapping into other processes (MapViewOfFile3).
type SectionRange struct {
	// Section is the handle of the section object (owned by the MemoryFile).
	// It stays valid only while the range is in use: a section is closed
	// when all its pages have been freed and decommitted.
	Section windows.Handle
	// SectionOffset is the offset of the range in the section.
	SectionOffset uint64
	// FileOffset is the offset of the range in the MemoryFile.
	FileOffset uint64
	// Length is the length of the range in bytes.
	Length uint64
}

// Sections returns the section ranges that back fr, one per 2 MiB extent fr
// intersects. Offsets and lengths are multiples of the page size if fr is
// page-aligned, which is all MapViewOfFile3 with placeholders needs (S2).
func (f *MemoryFile) Sections(fr memmap.FileRange) []SectionRange {
	var srs []SectionRange
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		chunk.hostEnsure(chunkFR)
		for off := chunkFR.Start; off < chunkFR.End; {
			e := (off & chunkMask) >> extentShift
			end := min(chunkFR.End, (off&^uint64(extentMask))+extentSize)
			srs = append(srs, SectionRange{
				Section:       windows.Handle(atomic.LoadUintptr(&chunk.ext.sec[e])),
				SectionOffset: off & extentMask,
				FileOffset:    off,
				Length:        end - off,
			})
			off = end
		}
		return true
	})
	return srs
}
