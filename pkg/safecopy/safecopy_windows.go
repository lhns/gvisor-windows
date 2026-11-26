// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows && amd64

package safecopy

import (
	"fmt"
	"syscall"
)

// The Windows counterpart of the Linux SIGSEGV/SIGBUS handler: a vectored
// exception handler (vehHandler, raw assembly) that, for an access violation
// or in-page error whose RIP lies inside one of the copy/zero/atomic routines,
// redirects RIP to the routine's fault stub with the fault address in AX and
// the signal number (11 for access violations, 7 for in-page errors) in DI,
// exactly as the Linux handler does. Other exceptions continue the search, so
// the Go runtime's own handler still sees them.

// vehHandler is the handler, using the Windows x64 calling convention.
func vehHandler()

// addrOfVEHHandler returns the address of the ABI0 vehHandler.
func addrOfVEHHandler() uintptr

func installFaultHandlers() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	r, _, err := k32.NewProc("AddVectoredExceptionHandler").Call(1 /* first */, addrOfVEHHandler())
	if r == 0 {
		panic(fmt.Sprintf("Unable to install the vectored exception handler: %v", err))
	}
}

// ensureFaultStack makes sure that the goroutine stack has room below the
// caller for the exception dispatcher. Windows runs vectored exception
// handlers on the faulting thread's current stack, which for a goroutine is
// a small heap-allocated stack: the kernel pushes a CONTEXT with the extended
// (AVX-512, AMX) state there, which can be several KB, and dispatching
// through ntdll needs more. Overflowing the stack corrupts neighbouring heap
// memory. The function is assembly with a large frame and no body, so it
// costs only the prologue's stack check.
func ensureFaultStack()

// dbgCtx is DEBUG: RIP, R8, RCX, RDI and the fault address of the last fault.
var dbgCtx [5]uint64
