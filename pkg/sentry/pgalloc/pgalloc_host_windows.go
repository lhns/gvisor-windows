// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

// The Windows host layer of MemoryFile. Instead of a memfd and mmap, every
// 1 GB chunk is a pagefile-backed section created with SEC_RESERVE and mapped
// once into the Sentry. See docs/results/1b-sentry-host-basics.md.
//
//   - SEC_RESERVE, not SEC_COMMIT: SEC_COMMIT charges the whole section to the
//     system commit limit at creation. With SEC_RESERVE, pages are committed
//     (VirtualAlloc MEM_COMMIT, shared by all views of the section) when they
//     are allocated; touching an uncommitted page is an access violation.
//   - One section per chunk keeps the prototype PTE allocation per section
//     bounded and lets the file grow by creating more sections.
//   - Mapped views cannot be decommitted (VirtualFree MEM_DECOMMIT fails), so
//     Decommit zeroes with DiscardVirtualMemory, which releases the physical
//     pages, and checks that the pages read as zero afterwards.
package pgalloc

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

const secReserve = 0x4000000 // SEC_RESERVE

var (
	modKernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procDiscardVirtualMem = modKernel32.NewProc("DiscardVirtualMemory")
	procQueryWorkingSetEx = modKernel32.NewProc("K32QueryWorkingSetEx")
)

// hostState is the Windows-specific state of a MemoryFile.
type hostState struct {
	// enabled is set by hostInit. A MemoryFile built without NewMemoryFile
	// (as in tests of the allocator) has no backing sections.
	enabled bool
}

// hostInit takes ownership of file, which is not used: the backing store is
// the chunk sections. file may be nil.
func (f *MemoryFile) hostInit() error {
	if f.file != nil {
		f.file.Close()
		f.file = nil
	}
	f.host.enabled = true
	return nil
}

// hostGrow creates and maps a section for each new chunk and records them in
// chunks.
func (f *MemoryFile) hostGrow(oldNrChunks, newNrChunks uint64, huge bool, chunks []chunkInfo) error {
	if !f.host.enabled {
		return nil
	}
	for i := oldNrChunks; i < newNrChunks; i++ {
		h, err := windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE|secReserve, uint32(chunkSize>>32), uint32(chunkSize&0xffffffff), nil)
		if err != nil {
			f.hostUnmapChunks(chunks[oldNrChunks:i])
			log.Warningf("CreateFileMapping for MemoryFile chunk %d failed: %v", i, err)
			return linuxerr.ENOMEM
		}
		addr, err := windows.MapViewOfFile(h, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, chunkSize)
		if err != nil {
			windows.CloseHandle(h)
			f.hostUnmapChunks(chunks[oldNrChunks:i])
			log.Warningf("MapViewOfFile for MemoryFile chunk %d failed: %v", i, err)
			return linuxerr.ENOMEM
		}
		chunks[i].mapping = addr
		chunks[i].section = uintptr(h)
	}
	return nil
}

func (f *MemoryFile) hostUnmapChunks(chunks []chunkInfo) {
	for i := range chunks {
		chunk := &chunks[i]
		if chunk.mapping != 0 {
			if err := windows.UnmapViewOfFile(chunk.mapping); err != nil {
				log.Warningf("Failed to unmap mapping %#x for MemoryFile chunk: %v", chunk.mapping, err)
			}
			chunk.mapping = 0
		}
		if chunk.section != 0 {
			windows.CloseHandle(windows.Handle(chunk.section))
			chunk.section = 0
		}
	}
}

func (f *MemoryFile) hostDestroy() {
	f.hostUnmapChunks(f.chunksLoad())
}

// hostPrepare commits the pages in fr. Committing is shared by all views of
// the section and idempotent; newly committed pages read as zero.
func (f *MemoryFile) hostPrepare(fr memmap.FileRange) error {
	if !f.host.enabled {
		return nil
	}
	var err error
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		addr := chunk.mapping + uintptr(chunkFR.Start&chunkMask)
		if _, e := windows.VirtualAlloc(addr, uintptr(chunkFR.Length()), windows.MEM_COMMIT, windows.PAGE_READWRITE); e != nil {
			log.Warningf("Failed to commit %v of MemoryFile: %v", chunkFR, e)
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

// decommitFile makes the committed pages in fr read as zero and releases
// their physical memory. It never fails: pages that DiscardVirtualMemory did
// not zero (it documents the contents as undefined) are cleared.
func (f *MemoryFile) decommitFile(fr memmap.FileRange) error {
	if !f.host.enabled {
		return nil
	}
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		base := chunk.mapping + uintptr(chunkFR.Start&chunkMask)
		end := base + uintptr(chunkFR.Length())
		for addr := base; addr < end; {
			var mbi windows.MemoryBasicInformation
			if err := windows.VirtualQuery(addr, &mbi, unsafe.Sizeof(mbi)); err != nil {
				log.Warningf("VirtualQuery(%#x) failed: %v", addr, err)
				return true
			}
			regionEnd := min(mbi.BaseAddress+mbi.RegionSize, end)
			if mbi.State == windows.MEM_COMMIT {
				discardOrClear(addr, regionEnd-addr)
			}
			addr = regionEnd
		}
		return true
	})
	return nil
}

func discardOrClear(addr, length uintptr) {
	bs := unsafe.Slice((*byte)(unsafe.Pointer(addr)), length)
	if r, _, _ := procDiscardVirtualMem.Call(addr, length); r == 0 {
		zero := true
		for i := 0; i+8 <= len(bs); i += 8 {
			if *(*uint64)(unsafe.Pointer(&bs[i])) != 0 {
				zero = false
				break
			}
		}
		if zero {
			return
		}
	}
	clear(bs)
}

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

// SectionRange describes the part of a MemoryFile chunk section that backs a
// file range, for mapping into other processes (MapViewOfFile3).
type SectionRange struct {
	// Section is the handle of the section object (owned by the MemoryFile).
	Section windows.Handle
	// SectionOffset is the offset of the range in the section.
	SectionOffset uint64
	// FileOffset is the offset of the range in the MemoryFile.
	FileOffset uint64
	// Length is the length of the range in bytes.
	Length uint64
}

// Sections returns the section ranges that back fr, one per chunk fr
// intersects. Offsets and lengths are multiples of the page size if fr is
// page-aligned, which is all MapViewOfFile3 with placeholders needs (S2).
func (f *MemoryFile) Sections(fr memmap.FileRange) []SectionRange {
	var srs []SectionRange
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		srs = append(srs, SectionRange{
			Section:       windows.Handle(chunk.section),
			SectionOffset: chunkFR.Start & chunkMask,
			FileOffset:    chunkFR.Start,
			Length:        chunkFR.Length(),
		})
		return true
	})
	return srs
}
