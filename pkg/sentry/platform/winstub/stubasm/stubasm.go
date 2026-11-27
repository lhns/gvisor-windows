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
// (trap table, trampolines, veneers) lives in the free gap [0xC0000,0x200000) (Windows maps
// more below: [0x10000,0xA4000) and, depending on the build, a few pages at 0xA0000 or 0xB0000), which
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

	TrapPtrs     uint64 = 0xC0000 // 8-byte pointers, one per patched site (jmp *abs32 targets)
	TrapPtrsSize uint64 = 0x10000
	TrapCode     uint64 = 0xD0000 // 32-byte entries: mov $nr,%eax; movabs $ret,%rcx; jmp tramp
	TrapCodeSize uint64 = 0x40000
	EntrySize    uint64 = 32
	MaxSites            = int(TrapPtrsSize / 8)

	StubCode     uint64 = 0x110000 // tramp at +0, trampWait at +0x400, threadEntry at +0x800
	StubCodeSize uint64 = 0x2000
	OffTramp     uint64 = 0
	OffTrampWait uint64 = 0x400
	OffThreadEnt uint64 = 0x800
	OffTrampPark uint64 = 0xC00
	OffICB       uint64 = 0x1000 // tripwire: the instrumentation callback
	OffICBInit   uint64 = 0x1400 // and the anchor's first code that registers it
	OffGetcpu    uint64 = 0x1800 // the vDSO's getcpu without a trap (GetcpuCode)

	Veneers     uint64 = 0x120000 // displaced instructions of relocated syscall sites
	VeneersSize uint64 = 0x70000
	// Veneers whose syscall returns inside them live in the upper part, at
	// addresses chosen platform-wide (the same in every address space).
	VeneersShared uint64 = 0x158000

	// Veneers of %fs instructions (rewrite mode, ADR 0020), private to the address space.
	FSVeneers     uint64 = 0x190000
	FSVeneersSize uint64 = 0x70000

	// Rewrite mode: the %fs veneers have a large area of their own, committed
	// as it fills, inside the guest range (reserved against the guest in the
	// memory manager). It is within rel32 reach of code loaded below about
	// 3 GiB (executables load at 0x1000_0000 and up, see arch.PIELoadBase).
	FSBig      uint64 = 0x4000_0000
	FSBigSize  uint64 = 32 << 20
	FSBigChunk uint64 = 64 << 10

	AreaLo uint64 = TrapPtrs // the low stub area is one section of this range
	AreaHi uint64 = FSVeneers + FSVeneersSize

	// Per-thread regions: a view of a per-process control section (not guest memory).
	CtlBase      uint64 = 0x6FFF_8000_0000
	ThreadRegion uint64 = 0x4000 // page, xsave area (at XsaveOff), stub stack
	MaxThreads          = 1024
	XsaveOff            = 0x1000
	XsaveSize           = 0x1000
	CtlSize      uint64 = ThreadRegion * uint64(MaxThreads)
)

// TEB slots the stub owns in rewrite mode (ADR 0020): TlsSlots[1] and [2], which a
// process without a CRT never touches. TebSave holds the guest's scratch register
// while a veneer runs; TebFS holds the guest's %fs base.
const (
	TebSave uint32 = 0x1488
	TebFS   uint32 = 0x1490
)

// Per-thread page offsets (the first 4 KB of each thread region; the rest is the
// stub-owned stack used for the slow wait path).
const (
	PgState     = 0x00 // uint32: 0 guest running, 1 request pending, 2 reply ready
	PgReply     = 0x08
	PgFS        = 0x10 // guest %fs base, written to the CPU on every return
	PgRIP       = 0x18 // resume address
	PgScratch   = 0x20
	PgRetRIP    = 0x28  // %fs repair trampoline return address
	PgCPU       = 0x30  // uint32: the task's CPU number, written by the Sentry on every resume (getcpu)
	PgStkTop    = 0x38  // 16-byte aligned top of the stub stack
	PgSpins     = 0x40  // pause iterations before the stub sleeps on the reply event
	PgCtlSlp    = 0x50  // uint32: the controller thread sleeps on the request event
	PgStubSlp   = 0x54  // uint32: the stub sleeps on the reply event
	PgHReq      = 0x58  // stub-side handle of the request event (stub signals)
	PgHReply    = 0x60  // stub-side handle of the reply event (stub waits)
	PgNtSet     = 0x68  // ntdll!NtSetEvent
	PgNtWait    = 0x70  // ntdll!NtWaitForSingleObject
	PgMode      = 0x78  // uint32: 0 return from a syscall, 1 restore the full context
	PgFlags     = 0x80  // rflags restored in mode 1
	PgXMask     = 0x88  // uint64: the xsave feature mask (XCR0 without AMX)
	PgRunning   = 0x9C  // uint32: the thread has left ntdll's start-up and entered threadEntry
	PgWaits     = 0x98  // uint32: how often the stub's wait for a reply returned (diagnostics)
	PgNtTerm    = 0x90  // ntdll!NtTerminateThread; mode 2 on a reply makes the thread end itself
	PgRegs      = 0x100 // 16 qwords in hardware order: rax rcx rdx rbx rsp rbp rsi rdi r8..r15
	PgICBInfo   = 0x1B0 // {version, reserved, callback}: the argument of NtSetInformationProcess
	PgICBStat   = 0x1C0 // its NTSTATUS
	PgICBTrip   = 0x1C8 // the guest address the callback caught
	PgNtSetInfo = 0x1D0 // ntdll!NtSetInformationProcess
	PgRepair    = 0x200 // per-thread %fs repair code
	PgDyn       = 0x400 // veneer of an %fs instruction the debugger runs (rewrite mode)
	PgDynSize   = 0x100
)

func trampPtr() unsafe.Pointer
func trampWaitPtr() unsafe.Pointer
func trampParkPtr() unsafe.Pointer
func icbPtr() unsafe.Pointer
func icbInitPtr() unsafe.Pointer
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

// Image lays the routines out at the StubCode offsets. rewrite: the CPU has no
// FSGSBASE, the guest's %fs base goes to the TEB (TebFS) instead of the CPU
// (ADR 0020); fxsave: no XSAVE, the vector state is saved with fxsave64/fxrstor64.
func Image(rewrite, fxsave bool) []byte {
	img := image()
	replace := func(from, to []byte) {
		n := 0
		for i := 0; i+len(from) <= len(img); i++ {
			if string(img[i:i+len(from)]) == string(from) {
				copy(img[i:], to)
				n++
				i += len(from) - 1
			}
		}
		if n == 0 {
			panic("stubasm: instruction pattern not found")
		}
	}
	if rewrite {
		// wrfsbase %rax; nop4  ->  mov %rax,%gs:TebFS
		replace([]byte{0xF3, 0x48, 0x0F, 0xAE, 0xD0, 0x0F, 0x1F, 0x40, 0x00},
			[]byte{0x65, 0x48, 0x89, 0x04, 0x25, byte(TebFS & 0xFF), byte(TebFS >> 8), 0, 0})
	}
	if fxsave {
		replace([]byte{0x49, 0x0F, 0xAE, 0xA3, 0, 0x10, 0, 0}, []byte{0x49, 0x0F, 0xAE, 0x83, 0, 0x10, 0, 0})
		replace([]byte{0x49, 0x0F, 0xAE, 0xAB, 0, 0x10, 0, 0}, []byte{0x49, 0x0F, 0xAE, 0x8B, 0, 0x10, 0, 0})
	}
	return img
}

func image() []byte {
	img := make([]byte, StubCodeSize)
	for _, r := range []struct {
		off uint64
		p   unsafe.Pointer
	}{{OffTramp, trampPtr()}, {OffTrampWait, trampWaitPtr()}, {OffThreadEnt, threadEntryPtr()}, {OffTrampPark, trampParkPtr()}, {OffICB, icbPtr()}, {OffICBInit, icbInitPtr()}} {
		code := copyCode(r.p)
		if len(code) > 0x400 {
			panic("stubasm: routine larger than its slot")
		}
		copy(img[r.off:], code)
	}
	copy(img[OffGetcpu:], GetcpuCode())
	return img
}

// GetcpuCode is getcpu(cpu, node, cache) for the vDSO: it stores the CPU number
// the Sentry left in the thread page (PgCPU) and node 0 through the pointers that
// are not null, returns 0 in rax and continues at rcx (rcx and r11 are lost, as
// in a syscall; the flags too, which the vDSO's caller does not keep).
func GetcpuCode() []byte {
	return []byte{
		0x65, 0x4C, 0x8B, 0x1C, 0x25, 0x28, 0, 0, 0, // mov %gs:0x28,%r11 (the thread page)
		0x41, 0x8B, 0x43, PgCPU, // mov PgCPU(%r11),%eax
		0x48, 0x85, 0xFF, // test %rdi,%rdi
		0x74, 0x02, // jz +2
		0x89, 0x07, // mov %eax,(%rdi)
		0x48, 0x85, 0xF6, // test %rsi,%rsi
		0x74, 0x06, // jz +6
		0xC7, 0x06, 0, 0, 0, 0, // movl $0,(%rsi)
		0x31, 0xC0, // xor %eax,%eax
		0xFF, 0xE1, // jmp *%rcx
	}
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
