#include "textflag.h"

// Windows x64 ABI vectored handler: LONG handler(EXCEPTION_POINTERS *ep) in CX.
// Handles ILLEGAL_INSTRUCTION / ACCESS_VIOLATION / PRIV_INSTRUCTION while vehSkip != 0:
// records code, RIP-advances by vehSkip, optionally samples FSBASE.
TEXT ·vehStub(SB),NOSPLIT|NOFRAME,$0
	MOVQ ·vehSkip(SB), DX
	TESTQ DX, DX
	JZ   search
	MOVQ 0(CX), R8
	MOVL 0(R8), R8
	MOVQ R8, ·vehCode(SB)
	CMPL R8, $0xC000001D
	JEQ  ok
	CMPL R8, $0xC0000005
	JEQ  ok
	CMPL R8, $0xC0000096
	JEQ  ok
	JMP  search
ok:
	MOVQ 8(CX), R9
	MOVWQZX 0x3E(R9), R10
	MOVQ R10, ·vehSegFs(SB)
	MOVQ ·vehReadFs(SB), R10
	TESTQ R10, R10
	JZ   skipfs
	BYTE $0xF3; BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xC0 // rdfsbase r8
	MOVQ R8, ·vehFsBase(SB)
skipfs:
	ADDQ DX, 0xF8(R9)
	INCQ ·vehCount(SB)
	MOVQ $-1, AX
	RET
search:
	XORL AX, AX
	RET

TEXT ·vehAddr(SB),NOSPLIT,$0-8
	LEAQ ·vehStub(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·rdfsbase(SB),NOSPLIT,$0-8
	BYTE $0xF3; BYTE $0x48; BYTE $0x0F; BYTE $0xAE; BYTE $0xC0 // rdfsbase rax
	MOVQ AX, ret+0(FP)
	RET

TEXT ·wrfsbase(SB),NOSPLIT,$0-8
	MOVQ v+0(FP), AX
	BYTE $0xF3; BYTE $0x48; BYTE $0x0F; BYTE $0xAE; BYTE $0xD0 // wrfsbase rax
	RET

// mov rax, fs:[0]
TEXT ·fs0(SB),NOSPLIT,$0-8
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x04; BYTE $0x25; BYTE $0; BYTE $0; BYTE $0; BYTE $0
	MOVQ AX, ret+0(FP)
	RET

// mov fs, eax  (8E E0)
TEXT ·movfs(SB),NOSPLIT,$0-8
	MOVQ sel+0(FP), AX
	BYTE $0x8E; BYTE $0xE0
	RET

TEXT ·ud2(SB),NOSPLIT,$0-0
	BYTE $0x0F; BYTE $0x0B
	RET

// spin until *stop != 0, counting mismatches of fs:[0] vs expect; returns (iterations, bad)
TEXT ·spinfs(SB),NOSPLIT,$0-32
	MOVQ stop+0(FP), SI
	MOVQ expect+8(FP), DI
	XORQ BX, BX
	XORQ CX, CX
loop:
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x04; BYTE $0x25; BYTE $0; BYTE $0; BYTE $0; BYTE $0
	CMPQ AX, DI
	JEQ  good
	INCQ CX
good:
	INCQ BX
	MOVL 0(SI), DX
	TESTL DX, DX
	JZ   loop
	MOVQ BX, iters+16(FP)
	MOVQ CX, bad+24(FP)
	RET

// func cpuid(leaf, sub uint32) (a, b, c, d uint32)
TEXT ·cpuid(SB),NOSPLIT,$0-24
	MOVL leaf+0(FP), AX
	MOVL sub+4(FP), CX
	CPUID
	MOVL AX, a+8(FP)
	MOVL BX, b+12(FP)
	MOVL CX, c+16(FP)
	MOVL DX, d+20(FP)
	RET
