#include "textflag.h"

// rawcopy: plain REP MOVSB copy, no fault handling of its own. func rawcopy(dst, src, n uintptr)
TEXT ·rawcopy(SB),NOSPLIT,$0-24
	MOVQ dst+0(FP), DI
	MOVQ src+8(FP), SI
	MOVQ n+16(FP), CX
	REP; MOVSB
	RET

// safecopy: same copy, recovered by the raw VEH below (RIP redirected to safecopyFault).
// func safecopy(dst, src, n uintptr) (ok uintptr)
TEXT ·safecopy(SB),NOSPLIT,$0-32
	MOVQ dst+0(FP), DI
	MOVQ src+8(FP), SI
	MOVQ n+16(FP), CX
	REP; MOVSB
	MOVQ $1, ret+24(FP)
	RET

TEXT ·safecopyFault(SB),NOSPLIT,$0-32
	MOVQ $0, ret+24(FP)
	RET

// Windows-ABI VEH: on ACCESS_VIOLATION with RIP inside safecopy (first 40 bytes), redirect to safecopyFault.
TEXT ·vehStub(SB),NOSPLIT|NOFRAME,$0
	MOVQ 0(CX), R8
	MOVL 0(R8), R9
	CMPL R9, $0xC0000005
	JNE  search
	MOVQ 8(CX), R10
	MOVQ 0xF8(R10), R11
	LEAQ ·safecopy(SB), R8
	SUBQ R8, R11
	CMPQ R11, $40
	JAE  search
	LEAQ ·safecopyFault(SB), R8
	MOVQ R8, 0xF8(R10)
	MOVQ $-1, AX
	RET
search:
	XORL AX, AX
	RET

TEXT ·vehAddr(SB),NOSPLIT,$0-8
	LEAQ ·vehStub(SB), AX
	MOVQ AX, ret+0(FP)
	RET
