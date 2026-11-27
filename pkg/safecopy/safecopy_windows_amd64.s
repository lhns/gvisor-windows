// Copyright 2026 The gvisor-windows authors.
// Licensed under the Apache License, Version 2.0.

//go:build windows && amd64

#include "textflag.h"

// Offsets in EXCEPTION_RECORD and CONTEXT (winnt.h, x64).
#define ER_INFO1 0x28 // ExceptionInformation[1]: the faulting address
#define CTX_RAX  0x78
#define CTX_RCX  0x80
#define CTX_R8   0xB8
#define CTX_R12  0xD8
#define CTX_R13  0xE0
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
	// Windows clobbers R8 when it resumes the thread after a vectored handler
	// (it holds the stack pointer at the stub), but the stub needs R8 and CX
	// to tell whether REP STOSB faulted. Pass them in R12 and R13, which are
	// restored, and let winMemclrFault move them back.
	MOVQ CTX_R8(R10), R9
	MOVQ R9, CTX_R12(R10)
	MOVQ CTX_RCX(R10), R9
	MOVQ R9, CTX_R13(R10)
	LEAQ winMemclrFault(SB), DX
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

// winMemclrFault is the memclr fault stub on Windows: it restores R8 and CX
// from the registers the handler passed them in.
TEXT winMemclrFault(SB),NOSPLIT|NOFRAME,$0
	MOVQ R12, R8
	MOVQ R13, CX
	JMP handleMemclrFault(SB)

TEXT ·addrOfVEHHandler(SB),NOSPLIT,$0-8
	LEAQ ·vehHandler(SB), AX
	MOVQ AX, ret+0(FP)
	RET


// ensureFaultStack: the frame size makes the prologue grow the stack until
// 16 KB are free below the caller (plus the runtime's own headroom for
// system calls).
TEXT ·ensureFaultStackAsm(SB),0,$16384-0
	RET
