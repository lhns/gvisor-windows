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

//go:build windows && amd64

package winstub

import (
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	k32 = windows.NewLazySystemDLL("kernel32.dll")
	kb  = windows.NewLazySystemDLL("kernelbase.dll")
	ntd = windows.NewLazySystemDLL("ntdll.dll")

	pVirtualAlloc2    = kb.NewProc("VirtualAlloc2")
	pVirtualFreeEx    = k32.NewProc("VirtualFreeEx")
	pVirtualProtectEx = k32.NewProc("VirtualProtectEx")
	pVirtualQueryEx   = k32.NewProc("VirtualQueryEx")
	pMapViewOfFile3   = kb.NewProc("MapViewOfFile3")
	pUnmapViewOfFile2 = kb.NewProc("UnmapViewOfFile2")
	pCreateMapping    = k32.NewProc("CreateFileMappingW")
	pMapViewOfFile    = k32.NewProc("MapViewOfFile")
	pUnmapViewOfFile  = k32.NewProc("UnmapViewOfFile")
	pWaitDebugEvent   = k32.NewProc("WaitForDebugEvent")
	pContinueDebug    = k32.NewProc("ContinueDebugEvent")
	pGetCtx           = k32.NewProc("GetThreadContext")
	pSetCtx           = k32.NewProc("SetThreadContext")
	pSuspendThread    = k32.NewProc("SuspendThread")
	pResumeThread     = k32.NewProc("ResumeThread")
	pTerminateProcess = k32.NewProc("TerminateProcess")
	pTerminateThread  = k32.NewProc("TerminateThread")
	pCreateRemoteThr  = k32.NewProc("CreateRemoteThread")
	pCloseHandle      = k32.NewProc("CloseHandle")
	pCreateEventW     = k32.NewProc("CreateEventW")
	pSetEvent         = k32.NewProc("SetEvent")
	pWaitSingle       = k32.NewProc("WaitForSingleObject")
	pDuplicateHandle  = k32.NewProc("DuplicateHandle")
	pReadProcMem      = k32.NewProc("ReadProcessMemory")
	pNtSetEvent       = ntd.NewProc("NtSetEvent")
	pNtWaitSingle     = ntd.NewProc("NtWaitForSingleObject")
	pNtQueryInformationThread = ntd.NewProc("NtQueryInformationThread")
)

const (
	memCommit      = 0x1000
	memReserve     = 0x2000
	memRelease     = 0x8000
	memCoalesce    = 0x1
	memPlaceholder = 0x40000
	memReplacePH   = 0x4000
	memPreservePH  = 0x2
	memFree        = 0x10000
	pageNoAccess   = 1
	pageReadOnly   = 2
	pageReadWrite  = 4
	pageExecRead   = 0x20
	pageExecRW     = 0x40
	fileMapAllExec = 0xF001F | 0x20
	fileMapWrite   = 0x2
	fileMapRead    = 0x4
	dbgContinue    = 0x00010002
	dbgNotHandled  = 0x80010001
	ctxControl     = 0x100001
	ctxControlInt  = 0x100003
	infinite       = 0xFFFFFFFF
)

// ptr converts a numeric address returned by the OS into a pointer without
// tripping vet's unsafeptr check.
func ptr(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

var debugEnabled = os.Getenv("WINSTUB_DEBUG") != ""

func debugf(format string, a ...any) {
	if debugEnabled {
		log.Printf("winstub: "+format, a...)
	}
}

// call invokes a Windows API and returns its result and, if it reported failure
// (a zero result), the Win32 error.
//
//go:uintptrescapes
func call(p *windows.LazyProc, a ...uintptr) (uintptr, error) {
	r, _, e := p.Call(a...)
	if r == 0 {
		return 0, e
	}
	return r, nil
}

type mbi struct {
	Base, AllocBase uintptr
	AllocProt       uint32
	_               uint32
	Size            uintptr
	State, Protect  uint32
	Type            uint32
	_               uint32
}

type rng struct{ lo, hi uint64 }

func (r rng) String() string { return fmt.Sprintf("[%#x,%#x)", r.lo, r.hi) }

// queryUsed lists the non-free regions of a process in [lo,hi).
func queryUsed(h uintptr, lo, hi uint64) []rng {
	var used []rng
	addr := lo
	for addr < hi {
		var m mbi
		r, _, _ := pVirtualQueryEx.Call(h, uintptr(addr), uintptr(unsafe.Pointer(&m)), unsafe.Sizeof(m))
		if r == 0 {
			break
		}
		end := uint64(m.Base) + uint64(m.Size)
		if m.State != memFree {
			s := uint64(m.Base)
			if s < lo {
				s = lo
			}
			if n := len(used); n > 0 && used[n-1].hi == s {
				used[n-1].hi = end
			} else {
				used = append(used, rng{s, end})
			}
		}
		if end <= addr {
			break
		}
		addr = end
	}
	return used
}

// Protection bits of a view (the guest's access type).
const (
	protR = 1
	protW = 2
	protX = 4
)

func winProt(prot int) uintptr {
	r, w, x := prot&protR != 0, prot&protW != 0, prot&protX != 0
	switch {
	case x && w:
		return pageExecRW
	case x:
		return pageExecRead
	case w:
		return pageReadWrite
	case r:
		return pageReadOnly
	}
	return pageNoAccess
}

// ctxBuf and debugEvent are handed to the OS as uintptr (base()), so they must
// not live on a goroutine stack: the stack can move between taking the address
// and the system call (a stack growth in the callee's prologue), and the OS would
// then write into the old stack, or the buffer would keep stale contents. The
// debugger loop of a stub hit this: WaitForDebugEvent filled a dead stack, the
// loop re-read the previous event, continued the wrong thread and the real event
// stayed unacknowledged, which froze the stub process for good (docs/results/1l).
// newCtxBuf and newDebugEvent return heap objects, which never move.
var (
	ctxSink atomic.Pointer[ctxBuf]
	evSink  atomic.Pointer[debugEvent]
)

//go:noinline
func newCtxBuf() *ctxBuf { c := new(ctxBuf); ctxSink.Store(c); return c }

//go:noinline
func newDebugEvent() *debugEvent { e := new(debugEvent); evSink.Store(e); return e }

// ctxBuf is a CONTEXT (amd64, 1232 bytes) with the 16-byte alignment the API needs.
type ctxBuf struct{ raw [1232 + 16]byte }

const (
	ctxFlagsOff  = 0x30
	ctxEFlagsOff = 0x44
	ctxRegsOff   = 0x78 // rax rcx rdx rbx rsp rbp rsi rdi r8..r15, hardware order
	ctxRipOff    = 0xF8
)

func (c *ctxBuf) base() uintptr {
	a := uintptr(unsafe.Pointer(&c.raw[0]))
	return (a + 15) &^ 15
}

func (c *ctxBuf) u64(off uintptr) *uint64 { return (*uint64)(ptr(c.base() + off)) }

func (c *ctxBuf) get(h uintptr, flags uint32) bool {
	*(*uint32)(ptr(c.base() + ctxFlagsOff)) = flags
	r, _, _ := pGetCtx.Call(h, c.base())
	return r != 0
}

func (c *ctxBuf) set(h uintptr) bool {
	r, _, _ := pSetCtx.Call(h, c.base())
	return r != 0
}

func (c *ctxBuf) eflags() uint32 { return *(*uint32)(ptr(c.base() + ctxEFlagsOff)) }

// debugEvent is a DEBUG_EVENT (x64).
type debugEvent struct{ raw [176 + 16]byte }

func (e *debugEvent) base() uintptr          { return (uintptr(unsafe.Pointer(&e.raw[0])) + 15) &^ 15 }
func (e *debugEvent) u32(off uintptr) uint32 { return *(*uint32)(ptr(e.base() + off)) }
func (e *debugEvent) u64(off uintptr) uint64 { return *(*uint64)(ptr(e.base() + off)) }
func (e *debugEvent) code() uint32           { return e.u32(0) }
func (e *debugEvent) pid() uint32            { return e.u32(4) }
func (e *debugEvent) tid() uint32            { return e.u32(8) }
func newEvent() uintptr {
	h, _, _ := pCreateEventW.Call(0, 0, 0, 0) // auto-reset, not signalled
	return h
}

// dupInto duplicates one of our handles into another process.
func dupInto(proc, h uintptr) uintptr {
	var d uintptr
	pDuplicateHandle.Call(^uintptr(0), h, proc, uintptr(unsafe.Pointer(&d)), 0, 0, 2)
	return d
}

const secReserveAttr = 0x4000000 // SEC_RESERVE

var pGetExitCode = k32.NewProc("GetExitCodeThread")

var pResetEvent = k32.NewProc("ResetEvent")

var pNtTerminateThread = ntd.NewProc("NtTerminateThread")

var pNtSetInfoProcess = ntd.NewProc("NtSetInformationProcess")

var pQueryThreadCycle = k32.NewProc("QueryThreadCycleTime")
