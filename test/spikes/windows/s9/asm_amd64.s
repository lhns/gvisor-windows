#include "textflag.h"

// Raw Windows-x64-ABI vectored handler (EXCEPTION_POINTERS* in CX), volatile registers only.
// While vehArmed != 0 it records into vehMarks (count, code, handler SP at entry, CONTEXT*,
// EXCEPTION_RECORD*, CONTEXT.Rsp, CONTEXT_EX All chunk, CONTEXT_EX XState chunk), moves RIP
// forward by 3 (the faulting load) and continues execution.
TEXT ·vehStub(SB),NOSPLIT|NOFRAME,$0
	MOVQ ·vehArmed(SB), AX
	TESTQ AX, AX
	JZ   search
	LEAQ ·vehMarks(SB), R10
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
	MOVQ 0x4D0(R11), AX
	MOVQ AX, 48(R10)
	MOVQ 0x4E0(R11), AX
	MOVQ AX, 56(R10)
	ADDQ $3, 0xF8(R11)
	MOVQ $-1, AX
	RET
search:
	XORL AX, AX
	RET

TEXT ·vehAddr(SB),NOSPLIT,$0-8
	LEAQ ·vehStub(SB), AX
	MOVQ AX, ret+0(FP)
	RET

// Input BX = mode (0 clean, 1 ymm dirty, 2 zmm0-31 and k1-k7 dirty). Clobbers AX.
TEXT dirty<>(SB),NOSPLIT|NOFRAME,$0
	CMPQ BX, $0
	JEQ  clean
	CMPQ BX, $1
	JEQ  ymm
	VPTERNLOGD $0xff, Z0, Z0, Z0
	VPTERNLOGD $0xff, Z1, Z1, Z1
	VPTERNLOGD $0xff, Z2, Z2, Z2
	VPTERNLOGD $0xff, Z3, Z3, Z3
	VPTERNLOGD $0xff, Z4, Z4, Z4
	VPTERNLOGD $0xff, Z5, Z5, Z5
	VPTERNLOGD $0xff, Z6, Z6, Z6
	VPTERNLOGD $0xff, Z7, Z7, Z7
	VPTERNLOGD $0xff, Z8, Z8, Z8
	VPTERNLOGD $0xff, Z9, Z9, Z9
	VPTERNLOGD $0xff, Z10, Z10, Z10
	VPTERNLOGD $0xff, Z11, Z11, Z11
	VPTERNLOGD $0xff, Z12, Z12, Z12
	VPTERNLOGD $0xff, Z13, Z13, Z13
	VPTERNLOGD $0xff, Z14, Z14, Z14
	VPTERNLOGD $0xff, Z15, Z15, Z15
	VPTERNLOGD $0xff, Z16, Z16, Z16
	VPTERNLOGD $0xff, Z17, Z17, Z17
	VPTERNLOGD $0xff, Z18, Z18, Z18
	VPTERNLOGD $0xff, Z19, Z19, Z19
	VPTERNLOGD $0xff, Z20, Z20, Z20
	VPTERNLOGD $0xff, Z21, Z21, Z21
	VPTERNLOGD $0xff, Z22, Z22, Z22
	VPTERNLOGD $0xff, Z23, Z23, Z23
	VPTERNLOGD $0xff, Z24, Z24, Z24
	VPTERNLOGD $0xff, Z25, Z25, Z25
	VPTERNLOGD $0xff, Z26, Z26, Z26
	VPTERNLOGD $0xff, Z27, Z27, Z27
	VPTERNLOGD $0xff, Z28, Z28, Z28
	VPTERNLOGD $0xff, Z29, Z29, Z29
	VPTERNLOGD $0xff, Z30, Z30, Z30
	VPTERNLOGD $0xff, Z31, Z31, Z31
	MOVL $-1, AX
	KMOVW AX, K1
	KMOVW AX, K2
	KMOVW AX, K3
	KMOVW AX, K4
	KMOVW AX, K5
	KMOVW AX, K6
	KMOVW AX, K7
	RET
ymm:
	VPCMPEQD Y0, Y0, Y0
	VPCMPEQD Y1, Y1, Y1
	VPCMPEQD Y2, Y2, Y2
	VPCMPEQD Y3, Y3, Y3
	VPCMPEQD Y4, Y4, Y4
	VPCMPEQD Y5, Y5, Y5
	VPCMPEQD Y6, Y6, Y6
	VPCMPEQD Y7, Y7, Y7
	VPCMPEQD Y8, Y8, Y8
	VPCMPEQD Y9, Y9, Y9
	VPCMPEQD Y10, Y10, Y10
	VPCMPEQD Y11, Y11, Y11
	VPCMPEQD Y12, Y12, Y12
	VPCMPEQD Y13, Y13, Y13
	VPCMPEQD Y14, Y14, Y14
	VPCMPEQD Y15, Y15, Y15
	RET
clean:
	VZEROALL
	RET

// func doFault(mode, newsp, lo, pat, drop uintptr, res *[4]uintptr)
// Dirties the vector state per mode, optionally switches SP to newsp (0: stay) and lowers it
// by drop, fills [lo, SP) with pat, raises an access violation (load from 0x10, handled by
// vehStub), then scans [lo, SP) upwards. res[0] = SP at the fault, res[1] = lowest changed
// address (0 if none). Uses only R12/R13/SI/DI/BX across the fault (Windows keeps them).
TEXT ·doFault(SB),NOSPLIT|NOFRAME,$0-48
	MOVQ mode+0(FP), BX
	MOVQ newsp+8(FP), R10
	MOVQ lo+16(FP), R12
	MOVQ pat+24(FP), R13
	MOVQ drop+32(FP), R11
	MOVQ res+40(FP), SI
	CALL dirty<>(SB)
	MOVQ SP, BX
	TESTQ R10, R10
	JZ   keep
	MOVQ R10, SP
keep:
	SUBQ R11, SP
	MOVQ SP, 0(SI)
	MOVQ R12, DI
	MOVQ SP, CX
	SUBQ DI, CX
	MOVQ R13, AX
	CLD
	REP; STOSB
	MOVQ $1, ·vehArmed(SB)
	MOVQ $0x10, AX
	BYTE $0x48; BYTE $0x8B; BYTE $0x10 // MOVQ (AX), DX
	MOVQ $0, ·vehArmed(SB)
	MOVQ R12, DI
	MOVQ SP, CX
	MOVQ R13, AX
scan:
	CMPQ DI, CX
	JAE  none
	CMPB 0(DI), AL
	JNE  found
	INCQ DI
	JMP  scan
none:
	XORL DI, DI
found:
	MOVQ DI, 8(SI)
	MOVQ BX, SP
	RET

// func stackLo() uintptr: g.stack.lo, reading g from R14 (ABIInternal g register, intact on
// entry to an ABI0 function called from Go).
TEXT ·stackLo(SB),NOSPLIT,$0-8
	MOVQ 0(R14), AX
	MOVQ AX, ret+0(FP)
	RET

// func cpuid(eax, ecx uint32) (a, b, c, d uint32)
TEXT ·cpuid(SB),NOSPLIT,$0-24
	MOVL eax+0(FP), AX
	MOVL ecx+4(FP), CX
	CPUID
	MOVL AX, a+8(FP)
	MOVL BX, b+12(FP)
	MOVL CX, c+16(FP)
	MOVL DX, d+20(FP)
	RET

// func xgetbv0() uint64
TEXT ·xgetbv0(SB),NOSPLIT,$0-8
	XORL CX, CX
	XGETBV
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ret+0(FP)
	RET
