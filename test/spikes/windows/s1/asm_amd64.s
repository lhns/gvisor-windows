#include "textflag.h"

// Windows-ABI VEH: for EXCEPTION_ILLEGAL_INSTRUCTION advance RIP by 2 (ud2) and continue.
TEXT ·vehStub(SB),NOSPLIT|NOFRAME,$0
	MOVQ 0(CX), R8
	MOVL 0(R8), R8
	CMPL R8, $0xC000001D
	JNE  search
	MOVQ 8(CX), R9
	ADDQ $2, 0xF8(R9)
	MOVQ $-1, AX
	RET
search:
	XORL AX, AX
	RET

TEXT ·vehAddr(SB),NOSPLIT,$0-8
	LEAQ ·vehStub(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·ud2loop(SB),NOSPLIT,$0-8
	MOVQ n+0(FP), CX
l:
	BYTE $0x0F; BYTE $0x0B
	DECQ CX
	JNZ  l
	RET

TEXT ·rdtsc(SB),NOSPLIT,$0-8
	RDTSC
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ret+0(FP)
	RET
