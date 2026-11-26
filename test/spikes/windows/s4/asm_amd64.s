#include "textflag.h"

// func linuxsys(n, a1, a2, a3, a4, a5, a6 uintptr) uintptr
// Executes a raw `syscall` with Linux register assignment (rdi,rsi,rdx,r10,r8,r9, rax=nr).
TEXT ·linuxsys(SB),NOSPLIT,$0-64
	MOVQ n+0(FP), AX
	MOVQ a1+8(FP), DI
	MOVQ a2+16(FP), SI
	MOVQ a3+24(FP), DX
	MOVQ a4+32(FP), R10
	MOVQ a5+40(FP), R8
	MOVQ a6+48(FP), R9
	SYSCALL
	MOVQ AX, ret+56(FP)
	RET
