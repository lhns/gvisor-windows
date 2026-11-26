#include "textflag.h"

// Raw Windows-x64-ABI vectored handler (LONG handler(EXCEPTION_POINTERS*) in CX).
// Only volatile registers (AX CX DX R8-R11) are used.
//
// 1. ACCESS_VIOLATION whose fault address is < 0x10000 or non-user (>= 0x8000_0000_0000)
//    on a thread with a registered expected FS base (fsTable[(tid>>2)&255]) whose
//    current FS base differs: records diagnostics, re-executes wrfsbase with the
//    expected base, returns EXCEPTION_CONTINUE_EXECUTION (the instruction is retried).
// 2. ILLEGAL_INSTRUCTION while vehSkip != 0: counts, samples FS base seen inside the
//    handler (vehFsSeen, vehLostAtDispatch), advances RIP by vehSkip.
// Everything else: EXCEPTION_CONTINUE_SEARCH (0).
TEXT ·vehStub(SB),NOSPLIT|NOFRAME,$0
	MOVQ 0(CX), R8
	MOVL 0(R8), R9
	CMPL R9, $0xC0000005
	JEQ  av
	CMPL R9, $0xC000001D
	JEQ  ud
	JMP  search
ud:
	MOVQ ·vehSkip(SB), DX
	TESTQ DX, DX
	JZ   search
	MOVQ 8(CX), R10
	MOVQ ·fsUsable(SB), AX
	TESTQ AX, AX
	JNZ  udfs
	INCQ ·vehCount(SB)
	ADDQ DX, 0xF8(R10)
	MOVQ $-1, AX
	RET
udfs:
	BYTE $0xF3; BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xC3 // rdfsbase r11
	MOVQ 0x48(GS), AX
	SHRQ $2, AX
	ANDQ $255, AX
	LEAQ ·fsTable(SB), R9
	MOVQ (R9)(AX*8), R9
	MOVQ R11, ·vehFsSeen(SB)
	CMPQ R9, R11
	JEQ  udok
	INCQ ·vehLostAtDispatch(SB)
udok:
	INCQ ·vehCount(SB)
	ADDQ DX, 0xF8(R10)
	MOVQ $-1, AX
	RET
av:
	MOVQ 0x28(R8), DX
	CMPQ DX, $0x10000
	JB   chk
	MOVQ $0x0000800000000000, AX
	CMPQ DX, AX
	JB   search
chk:
	MOVQ 0x48(GS), AX
	SHRQ $2, AX
	ANDQ $255, AX
	LEAQ ·fsTable(SB), R9
	MOVQ (R9)(AX*8), R9
	TESTQ R9, R9
	JZ   search
	BYTE $0xF3; BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xC3 // rdfsbase r11
	CMPQ R11, R9
	JEQ  search
	MOVQ DX, ·lastFaultAddr(SB)
	MOVQ R11, ·lastBaseAtLoss(SB)
	MOVQ 8(CX), R10
	MOVWQZX 0x3E(R10), AX
	MOVQ AX, ·lastSelCtx(SB)
	BYTE $0x8C; BYTE $0xE0 // mov eax, fs
	MOVQ AX, ·lastSelHW(SB)
	MOVQ ·selExpected(SB), DX
	CMPQ AX, DX
	JEQ  selok
	INCQ ·selMismatch(SB)
selok:
	BYTE $0xF3; BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xD1 // wrfsbase r9
	MOVQ $1, DX
	LOCK
	XADDQ DX, ·repairs(SB)
	MOVQ $-1, AX
	RET
search:
	XORL AX, AX
	RET

TEXT ·vehAddr(SB),NOSPLIT,$0-8
	LEAQ ·vehStub(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·tid(SB),NOSPLIT,$0-8
	MOVQ 0x48(GS), AX
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

TEXT ·selfs(SB),NOSPLIT,$0-8
	BYTE $0x8C; BYTE $0xE0 // mov eax, fs
	MOVQ AX, ret+0(FP)
	RET

// mov rax, fs:[0]
TEXT ·fs0(SB),NOSPLIT,$0-8
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x04; BYTE $0x25; BYTE $0; BYTE $0; BYTE $0; BYTE $0
	MOVQ AX, ret+0(FP)
	RET

TEXT ·ud2(SB),NOSPLIT,$0-0
	BYTE $0x0F; BYTE $0x0B
	RET

TEXT ·rdtsc(SB),NOSPLIT,$0-8
	RDTSC
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ret+0(FP)
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

// func guestloop(stop *uint32, exp *[3]uint64) (iters, bad uint64)
// Guest-like loop: reads fs:[0], fs:[0x28], fs:[-8] and compares with exp[0..2]
// until *stop != 0.
TEXT ·guestloop(SB),NOSPLIT,$0-32
	MOVQ stop+0(FP), SI
	MOVQ exp+8(FP), DI
	XORQ BX, BX
	XORQ CX, CX
loop:
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x04; BYTE $0x25; BYTE $0x00; BYTE $0; BYTE $0; BYTE $0 // mov rax, fs:[0]
	CMPQ AX, 0(DI)
	JEQ  a
	INCQ CX
a:
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x04; BYTE $0x25; BYTE $0x28; BYTE $0; BYTE $0; BYTE $0 // mov rax, fs:[0x28]
	CMPQ AX, 8(DI)
	JEQ  b
	INCQ CX
b:
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x04; BYTE $0x25; BYTE $0xF8; BYTE $0xFF; BYTE $0xFF; BYTE $0xFF // mov rax, fs:[-8]
	CMPQ AX, 16(DI)
	JEQ  c
	INCQ CX
c:
	INCQ BX
	MOVL 0(SI), DX
	TESTL DX, DX
	JZ   loop
	MOVQ BX, iters+16(FP)
	MOVQ CX, bad+24(FP)
	RET

// func loseRead(n uint64, off uint64): n times { wrfsbase 0; read fs:[0] (off=0) or fs:[-8] (off!=0) }
// The read faults, the VEH repairs the base and the read is retried.
TEXT ·loseRead(SB),NOSPLIT,$0-16
	MOVQ n+0(FP), CX
	MOVQ off+8(FP), DX
l2:
	XORL AX, AX
	BYTE $0xF3; BYTE $0x48; BYTE $0x0F; BYTE $0xAE; BYTE $0xD0 // wrfsbase rax
	TESTQ DX, DX
	JNZ  neg
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x04; BYTE $0x25; BYTE $0x00; BYTE $0; BYTE $0; BYTE $0
	JMP  next
neg:
	BYTE $0x64; BYTE $0x48; BYTE $0x8B; BYTE $0x04; BYTE $0x25; BYTE $0xF8; BYTE $0xFF; BYTE $0xFF; BYTE $0xFF
next:
	DECQ CX
	JNZ  l2
	RET
