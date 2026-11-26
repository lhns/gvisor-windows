// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows && amd64

#include "textflag.h"

// Offsets in EXCEPTION_RECORD and CONTEXT (winnt.h, x64).
#define ER_INFO1 0x28 // ExceptionInformation[1]: the faulting address
#define CTX_RAX  0x78
#define CTX_RDI  0xB0
#define CTX_RIP  0xF8

// vehHandler is a LONG CALLBACK taking an EXCEPTION_POINTERS* in CX. Win64
// callee-saved registers (DI, SI, BX, ...) must be preserved, so only AX, CX,
// DX and R8-R11 are used. It returns EXCEPTION_CONTINUE_EXECUTION (-1) after
// a fixup and EXCEPTION_CONTINUE_SEARCH (0) otherwise.
TEXT ·vehHandler(SB),NOSPLIT|NOFRAME,$0
	MOVQ 0(CX), R8       // EXCEPTION_RECORD*
	MOVQ 8(CX), R10      // CONTEXT*
	MOVL 0(R8), R9       // ExceptionCode
	MOVL $11, R11        // SIGSEGV
	CMPL R9, $0xC0000005 // STATUS_ACCESS_VIOLATION
	JEQ  check
	MOVL $7, R11         // SIGBUS
	CMPL R9, $0xC0000006 // STATUS_IN_PAGE_ERROR
	JNE  search

check:
	MOVQ CTX_RIP(R10), AX
	// DEBUG: record the first registers seen at the last fault.
	MOVQ AX, ·dbgCtx+0(SB)
	MOVQ 0xB8(R10), R9
	MOVQ R9, ·dbgCtx+8(SB)
	MOVQ 0x80(R10), R9
	MOVQ R9, ·dbgCtx+16(SB)
	MOVQ 0xB0(R10), R9
	MOVQ R9, ·dbgCtx+24(SB)
	MOVQ ER_INFO1(R8), R9
	MOVQ R9, ·dbgCtx+32(SB)

	CMPQ AX, ·memcpyBegin(SB)
	JB   not_memcpy
	CMPQ AX, ·memcpyEnd(SB)
	JAE  not_memcpy
	LEAQ handleMemcpyFault(SB), DX
	JMP  fix

not_memcpy:
	CMPQ AX, ·memclrBegin(SB)
	JB   not_memclr
	CMPQ AX, ·memclrEnd(SB)
	JAE  not_memclr
	LEAQ handleMemclrFault(SB), DX
	JMP  fix

not_memclr:
	CMPQ AX, ·swapUint32Begin(SB)
	JB   not_swap32
	CMPQ AX, ·swapUint32End(SB)
	JAE  not_swap32
	LEAQ handleSwapUint32Fault(SB), DX
	JMP  fix

not_swap32:
	CMPQ AX, ·swapUint64Begin(SB)
	JB   not_swap64
	CMPQ AX, ·swapUint64End(SB)
	JAE  not_swap64
	LEAQ handleSwapUint64Fault(SB), DX
	JMP  fix

not_swap64:
	CMPQ AX, ·compareAndSwapUint32Begin(SB)
	JB   not_cas32
	CMPQ AX, ·compareAndSwapUint32End(SB)
	JAE  not_cas32
	LEAQ handleCompareAndSwapUint32Fault(SB), DX
	JMP  fix

not_cas32:
	CMPQ AX, ·loadUint32Begin(SB)
	JB   not_load32
	CMPQ AX, ·loadUint32End(SB)
	JAE  not_load32
	LEAQ handleLoadUint32Fault(SB), DX
	JMP  fix

not_load32:
	CMPQ AX, ·checkXstateBegin(SB)
	JB   search
	CMPQ AX, ·checkXstateEnd(SB)
	JAE  search
	LEAQ handleCheckXstateFault(SB), DX

fix:
	// Resume in the fault stub with the address in AX and the signal in DI.
	MOVQ DX, CTX_RIP(R10)
	MOVQ ER_INFO1(R8), AX
	MOVQ AX, CTX_RAX(R10)
	MOVQ R11, CTX_RDI(R10)
	MOVQ $-1, AX
	RET

search:
	XORL AX, AX
	RET

TEXT ·addrOfVEHHandler(SB),NOSPLIT,$0-8
	LEAQ ·vehHandler(SB), AX
	MOVQ AX, ret+0(FP)
	RET

// ensureFaultStack: the frame size makes the prologue grow the stack until
// 16 KB are free below the caller (plus the runtime's own headroom for
// system calls).
TEXT ·ensureFaultStack(SB),0,$16384-0
	RET
