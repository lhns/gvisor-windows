#include "textflag.h"

// Raw Windows-x64-ABI vectored handler (EXCEPTION_POINTERS* in CX). Volatile registers only.
// While vehArmed != 0 it records into the shared mark block (vehMark[0..5] = count, code,
// handler SP at entry, CONTEXT*, EXCEPTION_RECORD*, CONTEXT.Rsp), advances RIP by vehSkip
// and continues. Otherwise EXCEPTION_CONTINUE_SEARCH.
TEXT ·vehStub(SB),NOSPLIT|NOFRAME,$0
	MOVQ ·vehArmed(SB), AX
	TESTQ AX, AX
	JZ   search
	MOVQ ·vehMark(SB), R10
	INCQ 0(R10)
	MOVQ SP, 16(R10)
	MOVQ 0(CX), R8
	MOVL 0(R8), R9
	MOVQ R9, 8(R10)
	MOVQ 8(CX), R11
	MOVQ R11, 24(R10)
	MOVQ R8, 32(R10)
	MOVQ 0x98(R11), AX
	MOVQ AX, 40(R10)
	MOVQ ·vehSkip(SB), DX
	ADDQ DX, 0xF8(R11)
	MOVQ $-1, AX
	RET
search:
	XORL AX, AX
	RET

TEXT ·vehAddr(SB),NOSPLIT,$0-8
	LEAQ ·vehStub(SB), AX
	MOVQ AX, ret+0(FP)
	RET

// func trap(kind, rsp uintptr)
// Switches RSP to the guest-like stack, raises one exception, switches back.
// kind: 0 ud2 (2 bytes), 1 load from 0x10 (3), 2 int3 (0), 3 div by zero (3),
//       4 store to [rsp-8] (5; faults when nothing is mapped below RSP)
// R12 holds the Go stack across the trap (preserved by the exception path).
TEXT ·trap(SB),NOSPLIT|NOFRAME,$0-16
	MOVQ kind+0(FP), BX
	MOVQ rsp+8(FP), DX
	MOVQ SP, R12
	MOVQ DX, SP
	MOVQ $0x10, AX
	CMPQ BX, $1
	JEQ  load
	CMPQ BX, $2
	JEQ  i3
	CMPQ BX, $3
	JEQ  dv
	CMPQ BX, $4
	JEQ  st
	BYTE $0x0F; BYTE $0x0B
	JMP  done
load:
	BYTE $0x48; BYTE $0x8B; BYTE $0x18
	JMP  done
i3:
	BYTE $0xCC
	JMP  done
dv:
	XORL CX, CX
	XORL DX, DX
	BYTE $0x48; BYTE $0xF7; BYTE $0xF1
	JMP  done
st:
	BYTE $0x48; BYTE $0x89; BYTE $0x44; BYTE $0x24; BYTE $0xF8
done:
	MOVQ R12, SP
	RET

// func marker(): ud2 then return (debugger skips 2 bytes)
TEXT ·marker(SB),NOSPLIT,$0-0
	BYTE $0x0F; BYTE $0x0B
	RET

// func thrEntry(): thread start routine, returns immediately (ExitThread via RtlUserThreadStart)
TEXT ·thrEntry(SB),NOSPLIT|NOFRAME,$0
	XORL AX, AX
	RET

TEXT ·thrEntryAddr(SB),NOSPLIT,$0-8
	LEAQ ·thrEntry(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·cpuid7(SB),NOSPLIT,$0-4
	MOVL $7, AX
	XORL CX, CX
	CPUID
	MOVL BX, ret+0(FP)
	RET

// func fsloop(n, base, magic uintptr) (bad uintptr)
// n times: wrfsbase 0, read fs:[0]; a debugger must restore the base for the read to return magic.
TEXT ·fsloop(SB),NOSPLIT,$0-32
	MOVQ n+0(FP), CX
	MOVQ base+8(FP), R8
	MOVQ magic+16(FP), R9
	XORL R10, R10
	BYTE $0xF3; BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xD0 // wrfsbase r8
l:
	XORL AX, AX
	BYTE $0xF3; BYTE $0x48; BYTE $0x0F; BYTE $0xAE; BYTE $0xD0 // wrfsbase rax (0)
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x1C; BYTE $0x25; BYTE $0; BYTE $0; BYTE $0; BYTE $0 // mov rbx, fs:[0]
	CMPQ BX, R9
	JEQ  ok
	INCQ R10
ok:
	DECQ CX
	JNZ  l
	MOVQ R10, bad+24(FP)
	RET
