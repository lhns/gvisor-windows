package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	k32 = syscall.NewLazyDLL("kernel32.dll")
	kb  = syscall.NewLazyDLL("kernelbase.dll")
	ntd = syscall.NewLazyDLL("ntdll.dll")

	pVirtualAlloc     = k32.NewProc("VirtualAlloc")
	pVirtualAlloc2    = kb.NewProc("VirtualAlloc2")
	pVirtualFreeEx    = k32.NewProc("VirtualFreeEx")
	pVirtualProtectEx = k32.NewProc("VirtualProtectEx")
	pVirtualQueryEx   = k32.NewProc("VirtualQueryEx")
	pMapViewOfFile3   = kb.NewProc("MapViewOfFile3")
	pUnmapViewOfFile2 = kb.NewProc("UnmapViewOfFile2")
	pCreateMapping    = k32.NewProc("CreateFileMappingW")
	pMapViewOfFile    = k32.NewProc("MapViewOfFile")
	pWaitDebugEvent   = k32.NewProc("WaitForDebugEvent")
	pContinueDebug    = k32.NewProc("ContinueDebugEvent")
	pOpenThread       = k32.NewProc("OpenThread")
	pGetCtx           = k32.NewProc("GetThreadContext")
	pSetCtx           = k32.NewProc("SetThreadContext")
	pSuspendThread    = k32.NewProc("SuspendThread")
	pResumeThread     = k32.NewProc("ResumeThread")
	pTerminateProcess = k32.NewProc("TerminateProcess")
	pTerminateThread  = k32.NewProc("TerminateThread")
	pCreateRemoteThr  = k32.NewProc("CreateRemoteThread")
	pCloseHandle      = k32.NewProc("CloseHandle")
	pNtDelay          = ntd.NewProc("NtDelayExecution")
	pUnmapViewOfFile  = k32.NewProc("UnmapViewOfFile")
)

const (
	memCommit       = 0x1000
	memReserve      = 0x2000
	memRelease      = 0x8000
	memFree         = 0x10000
	memPlaceholder  = 0x40000
	memReplacePH    = 0x4000
	memPreservePH   = 0x2
	pageNoAccess    = 1
	pageReadOnly    = 2
	pageReadWrite   = 4
	pageExecRead    = 0x20
	pageExecRW      = 0x40
	fileMapAllExec  = 0xF001F | 0x20
	dbgContinue     = 0x00010002
	dbgNotHandled   = 0x80010001
	ctxControl      = 0x100001
	ctxInteger      = 0x100002
	ctxControlInt   = 0x100003
	ctxFull         = 0x10000B
	threadAllAccess = 0x1FFFFF
)

// ptr converts a numeric address returned by the OS into a pointer without
// tripping vet's unsafeptr check.
func ptr(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

func valloc(size uintptr) uintptr {
	r, _, _ := pVirtualAlloc.Call(0, size, memCommit|memReserve, pageReadWrite)
	return r
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

func winProt(prot int) uintptr {
	r, w, x := prot&1 != 0, prot&2 != 0, prot&4 != 0
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
