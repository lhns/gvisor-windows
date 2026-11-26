// Copyright 2026 The gVisor Authors.
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

//go:build amd64

// Package stubasm holds the machine code that runs inside a stub process and the
// constants shared with the controller (stub address-space layout, per-thread
// shared page layout).
//
// The code is written in Go assembly so that no external assembler is needed. The
// controller copies the assembled bytes into the stub (Image). The routines use no
// absolute addresses except through the per-thread page found at %gs:0x28
// (TEB.ArbitraryUserPointer, free for application use) and, on the full-context
// return path, the TEB slot at %gs:0x1480 (TlsSlots[0]).
//
// Derived from the Phase 1a prototype (docs/results/1a-stub-prototype.md); the layout differs: the
// guest range starts above every region Windows maps at fixed low addresses.
package stubasm

import "unsafe"

// Stub address-space layout.
//
// Windows maps fixed regions at [0x10000,0x94000), [0xA0000,0xA2000),
// [0x200000,0x400000) and around 0x7FFE0000 in every process. The guest range
// therefore starts at 0x400000 (GuestMin). Everything below it that the stub needs
// (trap table, trampolines, veneers) lives in the free gap [0xA2000,0x200000), which
// is below 2 GiB so that `jmp *abs32` and `jmp rel32` from low guest code reach it.
const (
	ReserveMin uint64 = 0x10000          // placeholders start here
	ReserveMax uint64 = 0x7000_0000_0000 // and end here

	// Windows' loader allocates from the lowest free addresses; with the whole
	// range reserved it searches for ~200 ms before falling back. A free window
	// (also a hole in the guest range, reserved in the memory manager) fixes it:
	// 4 MiB is the least that works, thread stacks and TEBs need room later.
	WinWindow     uint64 = 0x2_0000_0000
	WinWindowSize uint64 = 64 << 20


	GuestMin uint64 = 0x400000
	GuestMax uint64 = CtlBase // first address the guest may not use

	TrapPtrs     uint64 = 0xB0000 // 8-byte pointers, one per patched site (jmp *abs32 targets)
	TrapPtrsSize uint64 = 0x10000
	TrapCode     uint64 = 0xC0000 // 32-byte entries: mov $nr,%eax; movabs $ret,%rcx; jmp tramp
	TrapCodeSize uint64 = 0x40000
	EntrySize    uint64 = 32
	MaxSites            = int(TrapPtrsSize / 8)

	StubCode     uint64 = 0x100000 // tramp at +0, trampWait at +0x400, threadEntry at +0x800
	StubCodeSize uint64 = 0x1000
	OffTramp     uint64 = 0
	OffTrampWait uint64 = 0x400
	OffThreadEnt uint64 = 0x800
	OffTrampPark uint64 = 0xC00

	Veneers     uint64 = 0x110000 // displaced instructions of relocated syscall sites
	VeneersSize uint64 = 0x70000
	// Veneers whose syscall returns inside them live in the upper part, at
	// addresses chosen platform-wide (the same in every address space).
	VeneersShared uint64 = 0x148000

	AreaLo uint64 = TrapPtrs // the low stub area is one section of this range
	AreaHi uint64 = Veneers + VeneersSize

	// Per-thread regions: a view of a per-process control section (not guest memory).
	CtlBase      uint64 = 0x6FFF_8000_0000
	ThreadRegion uint64 = 0x4000 // page, xsave area (at XsaveOff), stub stack
	MaxThreads          = 1024
	XsaveOff            = 0x1000
	XsaveSize           = 0x1000
	CtlSize      uint64 = ThreadRegion * uint64(MaxThreads)
)

// Per-thread page offsets (the first 4 KB of each thread region; the rest is the
// stub-owned stack used for the slow wait path).
const (
	PgState   = 0x00 // uint32: 0 guest running, 1 request pending, 2 reply ready
	PgReply   = 0x08
	PgFS      = 0x10 // guest %fs base, written to the CPU on every return
	PgRIP     = 0x18 // resume address
	PgScratch = 0x20
	PgRetRIP  = 0x28  // %fs repair trampoline return address
	PgStkTop  = 0x38  // 16-byte aligned top of the stub stack
	PgSpins   = 0x40  // pause iterations before the stub sleeps on the reply event
	PgCtlSlp  = 0x50  // uint32: the controller thread sleeps on the request event
	PgStubSlp = 0x54  // uint32: the stub sleeps on the reply event
	PgHReq    = 0x58  // stub-side handle of the request event (stub signals)
	PgHReply  = 0x60  // stub-side handle of the reply event (stub waits)
	PgNtSet   = 0x68  // ntdll!NtSetEvent
	PgNtWait  = 0x70  // ntdll!NtWaitForSingleObject
	PgMode    = 0x78  // uint32: 0 return from a syscall, 1 restore the full context
	PgFlags   = 0x80  // rflags restored in mode 1
	PgXMask   = 0x88  // uint64: the xsave feature mask (XCR0 without AMX)
	PgWaits   = 0x98  // uint32: how often the stub's wait for a reply returned (diagnostics)
	PgNtTerm  = 0x90  // ntdll!NtTerminateThread; mode 2 on a reply makes the thread end itself
	PgRegs    = 0x100 // 16 qwords in hardware order: rax rcx rdx rbx rsp rbp rsi rdi r8..r15
	PgRepair  = 0x200 // per-thread %fs repair code
)

func trampPtr() unsafe.Pointer
func trampWaitPtr() unsafe.Pointer
func trampParkPtr() unsafe.Pointer
func xcr0() uint64

// XSaveMask is the feature mask the stub saves and restores: everything the OS
// enabled except AMX tile state (which the Sentry does not support either).
func XSaveMask() uint64 { return xcr0() &^ 0x60000 }
func threadEntryPtr() unsafe.Pointer
func cpuid7() uint32

// HasFSGSBASE reports CPUID.7.0:EBX bit 0.
func HasFSGSBASE() bool { return cpuid7()&1 != 0 }

func copyCode(p unsafe.Pointer) []byte {
	b := unsafe.Slice((*byte)(p), 2048)
	sent := "\x0f\x0b\x0f\x0b\x0f\x0b\x0f\x0b"
	for i := 0; i+8 <= len(b); i++ {
		if string(b[i:i+8]) == sent {
			return append([]byte(nil), b[:i]...)
		}
	}
	panic("stubasm: sentinel not found")
}

// Image lays the routines out at the StubCode offsets.
func Image() []byte {
	img := make([]byte, StubCodeSize)
	for _, r := range []struct {
		off uint64
		p   unsafe.Pointer
	}{{OffTramp, trampPtr()}, {OffTrampWait, trampWaitPtr()}, {OffThreadEnt, threadEntryPtr()}, {OffTrampPark, trampParkPtr()}} {
		code := copyCode(r.p)
		if len(code) > 0x400 {
			panic("stubasm: routine larger than its slot")
		}
		copy(img[r.off:], code)
	}
	return img
}

// RepairCode returns the %fs repair trampoline for the thread page at pg: it
// restores %fs from the page, then continues at the address in PgRetRIP, keeping
// every register and flag (S8 method).
func RepairCode(pg uint64) []byte {
	u64 := func(v uint64) []byte {
		b := make([]byte, 8)
		for i := range b {
			b[i] = byte(v >> (8 * i))
		}
		return b
	}
	var c []byte
	c = append(c, 0x48, 0xA3) // mov [scratch], rax
	c = append(c, u64(pg+PgScratch)...)
	c = append(c, 0x48, 0xA1) // mov rax, [fs slot]
	c = append(c, u64(pg+PgFS)...)
	c = append(c, 0xF3, 0x48, 0x0F, 0xAE, 0xD0) // wrfsbase rax
	c = append(c, 0x48, 0xA1)                   // mov rax, [scratch]
	c = append(c, u64(pg+PgScratch)...)
	next := pg + PgRepair + uint64(len(c)) + 6
	rel := uint32(int32(int64(pg+PgRetRIP) - int64(next)))
	c = append(c, 0xFF, 0x25, byte(rel), byte(rel>>8), byte(rel>>16), byte(rel>>24)) // jmp [retrip]
	return c
}
