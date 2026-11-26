// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows

package pgalloc

import (
	"bytes"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func newTestMemoryFile(t *testing.T) *MemoryFile {
	t.Helper()
	f, err := NewMemoryFile(nil, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionManual,
		DecommitOnDestroy:       true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(f.Destroy)
	return f
}

func fillRange(t *testing.T, f *MemoryFile, fr memmap.FileRange, b byte) {
	t.Helper()
	bs, err := f.MapInternal(fr, hostarch.ReadWrite)
	if err != nil {
		t.Fatalf("MapInternal(%v): %v", fr, err)
	}
	for ; !bs.IsEmpty(); bs = bs.Tail() {
		s := bs.Head().ToSlice()
		for i := range s {
			s[i] = b
		}
	}
}

func checkRange(t *testing.T, f *MemoryFile, fr memmap.FileRange, b byte) {
	t.Helper()
	bs, err := f.MapInternal(fr, hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal(%v): %v", fr, err)
	}
	for ; !bs.IsEmpty(); bs = bs.Tail() {
		s := bs.Head().ToSlice()
		for i := range s {
			if s[i] != b {
				t.Fatalf("range %v: byte %d is %#x, want %#x", fr, i, s[i], b)
			}
		}
	}
}

func TestWindowsAllocateWriteDecommit(t *testing.T) {
	f := newTestMemoryFile(t)
	const size = 1 << 20
	for _, mode := range []AllocationMode{AllocateUncommitted, AllocateCallerIndirectCommit, AllocateAndCommit, AllocateAndWritePopulate} {
		fr, err := f.Allocate(size, AllocOpts{Kind: usage.Anonymous, Mode: mode})
		if err != nil {
			t.Fatalf("Allocate(mode %v): %v", mode, err)
		}
		checkRange(t, f, fr, 0)
		fillRange(t, f, fr, 0xab)
		checkRange(t, f, fr, 0xab)
		f.Decommit(fr)
		checkRange(t, f, fr, 0)
		f.DecRef(fr)
	}
}

func TestWindowsSectionViewSeesWrites(t *testing.T) {
	f := newTestMemoryFile(t)
	fr, err := f.Allocate(4*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	srs := f.Sections(fr)
	if len(srs) != 1 || srs[0].Length != fr.Length() {
		t.Fatalf("Sections(%v) = %+v", fr, srs)
	}
	sr := srs[0]
	// Views must start at the allocation granularity; map from the section
	// start and index into it.
	view, err := windows.MapViewOfFile(sr.Section, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, 0)
	if err != nil {
		t.Fatalf("MapViewOfFile: %v", err)
	}
	defer windows.UnmapViewOfFile(view)
	other := unsafe.Slice((*byte)(unsafe.Pointer(view+uintptr(sr.SectionOffset))), sr.Length)

	fillRange(t, f, fr, 0x5a)
	if !bytes.Equal(other, bytes.Repeat([]byte{0x5a}, len(other))) {
		t.Errorf("a second view of the section does not see writes through MapInternal")
	}
	// The second view's page is resident, so this checks that Decommit zeroes
	// pages that another view has mapped, as a stub process's view will.
	f.Decommit(fr)
	if !bytes.Equal(other, make([]byte, len(other))) {
		t.Errorf("a second view of the section does not read zeros after Decommit")
	}
	checkRange(t, f, fr, 0)
}

func TestWindowsGrowAcrossChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("commits more than 1 GB")
	}
	f := newTestMemoryFile(t)
	// Fill the first chunk except for its last page, then allocate a range
	// that straddles the chunk boundary.
	first, err := f.Allocate(chunkSize-hostarch.PageSize, AllocOpts{Kind: usage.Anonymous, Mode: AllocateUncommitted})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	fr, err := f.Allocate(2*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got := f.TotalSize(); got != 2*chunkSize {
		t.Fatalf("TotalSize = %#x, want %#x", got, 2*chunkSize)
	}
	if fr.Start != chunkSize-hostarch.PageSize {
		t.Fatalf("allocation at %#x does not straddle the chunk boundary", fr.Start)
	}
	if n := len(f.Sections(fr)); n != 2 {
		t.Fatalf("Sections returned %d ranges, want 2", n)
	}
	fillRange(t, f, fr, 0x77)
	checkRange(t, f, fr, 0x77)
	f.DecRef(fr)
	f.DecRef(first)
}

func TestWindowsTotalUsage(t *testing.T) {
	f := newTestMemoryFile(t)
	fr, err := f.Allocate(64*hostarch.PageSize, AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	fillRange(t, f, fr, 1)
	got, err := f.TotalUsage()
	if err != nil {
		t.Fatalf("TotalUsage: %v", err)
	}
	if got < fr.Length() {
		t.Errorf("TotalUsage = %d after touching %d bytes", got, fr.Length())
	}
}
