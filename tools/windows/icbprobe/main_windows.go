// icbprobe checks whether a process can register a process instrumentation callback
// (NtSetInformationProcess, class 40) on itself, as the current user, and whether the
// callback runs on returns from the kernel (docs/results/1m-platform-repair.md).
package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	ntdll    = syscall.NewLazyDLL("ntdll.dll")
	setInfo  = ntdll.NewProc("NtSetInformationProcess")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	valloc   = kernel32.NewProc("VirtualAlloc")
)

type icbInfo struct {
	Version  uint32
	Reserved uint32
	Callback uintptr
}

func main() {
	// callback: lock inc qword [rip+counter]; jmp r10 (r10 = the interrupted address)
	mem, _, err := valloc.Call(0, 4096, 0x3000, 0x40) // reserve+commit, RWX
	if mem == 0 {
		fmt.Println("VirtualAlloc:", err)
		os.Exit(2)
	}
	code := []byte{0xF0, 0x48, 0xFF, 0x05, 0xF4, 0x0F, 0, 0, 0x41, 0xFF, 0xE2} // counter at +0x1000 (rel from +8)
	// counter lives at mem+0x1000-... use mem+0x800: rel = 0x800 - 8
	rel := int32(0x800 - 8)
	code[4], code[5], code[6], code[7] = byte(rel), byte(rel>>8), byte(rel>>16), byte(rel>>24)
	copy(unsafe.Slice((*byte)(unsafe.Pointer(mem)), 64), code)
	counter := (*uint64)(unsafe.Pointer(mem + 0x800))

	for _, ver := range []uint32{0, 1} {
		info := icbInfo{Version: ver, Callback: mem}
		r, _, _ := setInfo.Call(^uintptr(0), 40, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
		fmt.Printf("NtSetInformationProcess(version %d) = %#x\n", ver, uint32(r))
		if r == 0 {
			break
		}
	}
	before := *counter
	for i := 0; i < 100; i++ {
		os.Getpid()
		syscall.Getpagesize()
		_, _ = os.Stat(os.Args[0])
	}
	fmt.Printf("callback hits: %d (after %d before)\n", *counter, before)
}
