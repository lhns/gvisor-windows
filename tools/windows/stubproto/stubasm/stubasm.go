// Package stubasm holds the machine code that runs inside a stub process and the
// constants shared with the controller (guest address-space layout, per-thread
// shared page layout).
//
// The code is written in Go assembly so that no external assembler is needed. The
// controller copies the assembled bytes into the guest (Image). The routines use no
// absolute addresses except through the per-thread page found at %gs:0x28
// (TEB.ArbitraryUserPointer, free for application use).
package stubasm

import "unsafe"

// Guest address-space layout (shared by the prototype and a future platform).
const (
	GuestMin uint64 = 0x10000
	GuestMax uint64 = 0x7000_0000_0000

	TrapPtrs     uint64 = 0x60000 // 8-byte pointers, one per patched site (jmp *abs32 targets)
	TrapPtrsSize uint64 = 0x8000
	TrapCode     uint64 = 0x70000 // 32-byte entries: mov $nr,%eax; movabs $ret,%rcx; jmp tramp
	TrapCodeSize uint64 = 0x20000
	EntrySize    uint64 = 32
	MaxSites            = int(TrapPtrsSize / 8)

	StubCode     uint64 = 0x90000 // tramp at +0, trampWait at +0x400, threadEntry at +0x800
	StubCodeSize uint64 = 0x1000
	OffTramp     uint64 = 0
	OffTrampWait uint64 = 0x400
	OffThreadEnt uint64 = 0x800

	// Per-thread regions: a view of a per-process control section (not guest memory).
	CtlBase      uint64 = 0x6FFF_8000_0000
	ThreadRegion uint64 = 0x10000
	MaxThreads          = 32
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
	PgRetRIP  = 0x28 // %fs repair trampoline return address
	PgNtDelay = 0x30 // ntdll!NtDelayExecution
	PgStkTop  = 0x38 // 16-byte aligned top of the stub stack
	PgSpins   = 0x40
	PgDelay   = 0x48  // LARGE_INTEGER relative timeout
	PgRegs    = 0x100 // 16 qwords in hardware order: rax rcx rdx rbx rsp rbp rsi rdi r8..r15
	PgRepair  = 0x200 // per-thread %fs repair code
)

func trampPtr() unsafe.Pointer
func trampWaitPtr() unsafe.Pointer
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
	copy(img[OffTramp:], copyCode(trampPtr()))
	copy(img[OffTrampWait:], copyCode(trampWaitPtr()))
	copy(img[OffThreadEnt:], copyCode(threadEntryPtr()))
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
