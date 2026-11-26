#include "textflag.h"

// Position-independent stub code. The Go linker places these routines in the
// controller binary; the controller copies their bytes (up to an 8-byte ud2
// sentinel) into the guest address space. They reference nothing but %gs:0x28
// (TEB.ArbitraryUserPointer = per-thread shared page) and the page itself.
// Page layout: see stubasm.go. Registers rax, rcx, r11 are free at a syscall
// site (the syscall instruction clobbers them); everything else is saved.

// Wait for state==2: spin, then sleep on the reply event (stubSleeping handshake),
// then restore the guest registers from the page, set the %fs base and return to the
// guest. Expects R11 = page, AX = spin budget.
#define WAITRET \
wait: \
	CMPL 0(R11), $2 \
	JEQ  done \
	PAUSE \
	DECQ AX \
	JNZ  wait \
	MOVQ 0x38(R11), SP \
sleep: \
	MOVL $1, AX \
	XCHGL AX, 0x54(R11) \
	CMPL 0(R11), $2 \
	JEQ  woke \
	MOVQ 0x60(R11), CX \
	XORL DX, DX \
	XORL R8, R8 \
	MOVQ 0x70(R11), AX \
	CALL AX \
	BYTE $0x65; BYTE $0x4C; BYTE $0x8B; BYTE $0x1C; BYTE $0x25; LONG $0x28 \
	JMP  sleep \
woke: \
	MOVL $0, 0x54(R11) \
done: \
	CMPL 0x78(R11), $2 \
	JEQ  die \
	MOVL $0, 0(R11) \
	MOVL 0x88(R11), AX \
	MOVL 0x8C(R11), DX \
	BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xAB; LONG $0x1000 \
	CMPL 0x78(R11), $0 \
	JNE  full \
	MOVQ 0x110(R11), DX \
	MOVQ 0x118(R11), BX \
	MOVQ 0x128(R11), BP \
	MOVQ 0x130(R11), SI \
	MOVQ 0x138(R11), DI \
	MOVQ 0x140(R11), R8 \
	MOVQ 0x148(R11), R9 \
	MOVQ 0x150(R11), R10 \
	MOVQ 0x160(R11), R12 \
	MOVQ 0x168(R11), R13 \
	MOVQ 0x170(R11), R14 \
	MOVQ 0x178(R11), R15 \
	MOVQ 0x120(R11), SP \
	MOVQ 0x10(R11), AX \
	BYTE $0xF3; BYTE $0x48; BYTE $0x0F; BYTE $0xAE; BYTE $0xD0 \
	MOVQ 0x08(R11), AX \
	MOVQ 0x18(R11), CX \
	MOVL $0x246, R11 \
	JMP  CX \
die: \
	MOVQ 0x38(R11), SP \
	MOVQ $-2, CX \
	XORL DX, DX \
	MOVQ 0x90(R11), AX \
	CALL AX \
	BYTE $0x0F; BYTE $0x0B \
full: \
	MOVQ 0x38(R11), SP \
	MOVQ 0x18(R11), AX \
	BYTE $0x65; BYTE $0x48; BYTE $0x89; BYTE $0x04; BYTE $0x25; LONG $0x1480 \
	PUSHQ 0x80(R11) \
	POPFQ \
	MOVQ 0x108(R11), CX \
	MOVQ 0x110(R11), DX \
	MOVQ 0x118(R11), BX \
	MOVQ 0x128(R11), BP \
	MOVQ 0x130(R11), SI \
	MOVQ 0x138(R11), DI \
	MOVQ 0x140(R11), R8 \
	MOVQ 0x148(R11), R9 \
	MOVQ 0x150(R11), R10 \
	MOVQ 0x160(R11), R12 \
	MOVQ 0x168(R11), R13 \
	MOVQ 0x170(R11), R14 \
	MOVQ 0x178(R11), R15 \
	MOVQ 0x10(R11), AX \
	BYTE $0xF3; BYTE $0x48; BYTE $0x0F; BYTE $0xAE; BYTE $0xD0 \
	MOVQ 0x08(R11), AX \
	MOVQ 0x120(R11), SP \
	MOVQ 0x158(R11), R11 \
	BYTE $0x65; BYTE $0xFF; BYTE $0x24; BYTE $0x25; LONG $0x1480

// Syscall entry from a trap-table entry: AX = number, CX = resume rip.
TEXT ·tramp(SB),NOSPLIT|NOFRAME,$0-0
	BYTE $0x65; BYTE $0x4C; BYTE $0x8B; BYTE $0x1C; BYTE $0x25; LONG $0x28
	MOVQ AX, 0x100(R11)
	MOVQ CX, 0x108(R11)
	MOVQ DX, 0x110(R11)
	MOVQ BX, 0x118(R11)
	MOVQ SP, 0x120(R11)
	MOVQ BP, 0x128(R11)
	MOVQ SI, 0x130(R11)
	MOVQ DI, 0x138(R11)
	MOVQ R8, 0x140(R11)
	MOVQ R9, 0x148(R11)
	MOVQ R10, 0x150(R11)
	MOVQ R12, 0x160(R11)
	MOVQ R13, 0x168(R11)
	MOVQ R14, 0x170(R11)
	MOVQ R15, 0x178(R11)
	MOVL 0x88(R11), AX
	MOVL 0x8C(R11), DX
	BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xA3; LONG $0x1000
	MOVQ CX, 0x18(R11)
	MOVL $1, AX
	XCHGL AX, 0(R11)
	CMPL 0x50(R11), $0
	JEQ  go
	MOVQ 0x38(R11), SP
	MOVQ 0x58(R11), CX
	XORL DX, DX
	MOVQ 0x68(R11), AX
	CALL AX
	BYTE $0x65; BYTE $0x4C; BYTE $0x8B; BYTE $0x1C; BYTE $0x25; LONG $0x28
go:
	MOVQ 0x40(R11), AX
	WAITRET
	BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B

// Entry for a ud2 site: the controller already stored registers and set
// state=1; the thread only waits for the reply.
TEXT ·trampWait(SB),NOSPLIT|NOFRAME,$0-0
	BYTE $0x65; BYTE $0x4C; BYTE $0x8B; BYTE $0x1C; BYTE $0x25; LONG $0x28
	MOVQ 0x40(R11), AX
	WAITRET
	BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B

// Thread start: CX = page. Loads the whole register file and %fs, then jumps.
TEXT ·threadEntry(SB),NOSPLIT|NOFRAME,$0-0
	MOVQ CX, R11
	BYTE $0x65; BYTE $0x4C; BYTE $0x89; BYTE $0x1C; BYTE $0x25; LONG $0x28
	MOVL 0x88(R11), AX
	MOVL 0x8C(R11), DX
	BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xAB; LONG $0x1000
	MOVQ 0x10(R11), AX
	BYTE $0xF3; BYTE $0x48; BYTE $0x0F; BYTE $0xAE; BYTE $0xD0
	MOVQ 0x108(R11), CX
	MOVQ 0x110(R11), DX
	MOVQ 0x118(R11), BX
	MOVQ 0x128(R11), BP
	MOVQ 0x130(R11), SI
	MOVQ 0x138(R11), DI
	MOVQ 0x140(R11), R8
	MOVQ 0x148(R11), R9
	MOVQ 0x150(R11), R10
	MOVQ 0x160(R11), R12
	MOVQ 0x168(R11), R13
	MOVQ 0x170(R11), R14
	MOVQ 0x178(R11), R15
	MOVQ 0x120(R11), SP
	MOVQ 0x100(R11), AX
	MOVQ 0x18(R11), R11
	JMP  R11
	BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B

// Entry for threads the debugger stopped (fault, interrupt, ud2 syscall): the
// debugger stored the general registers, flags and resume address in the page and
// redirected the thread here. Save the FP/vector state, publish the request and
// wait for the reply.
TEXT ·trampPark(SB),NOSPLIT|NOFRAME,$0-0
	BYTE $0x65; BYTE $0x4C; BYTE $0x8B; BYTE $0x1C; BYTE $0x25; LONG $0x28
	MOVL 0x88(R11), AX
	MOVL 0x8C(R11), DX
	BYTE $0x49; BYTE $0x0F; BYTE $0xAE; BYTE $0xA3; LONG $0x1000
	MOVL $1, AX
	XCHGL AX, 0(R11)
	CMPL 0x50(R11), $0
	JEQ  go
	MOVQ 0x38(R11), SP
	MOVQ 0x58(R11), CX
	XORL DX, DX
	MOVQ 0x68(R11), AX
	CALL AX
	BYTE $0x65; BYTE $0x4C; BYTE $0x8B; BYTE $0x1C; BYTE $0x25; LONG $0x28
go:
	MOVQ 0x40(R11), AX
	WAITRET
	BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B; BYTE $0x0F; BYTE $0x0B

TEXT ·trampParkPtr(SB),NOSPLIT,$0-8
	LEAQ ·trampPark(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·xcr0(SB),NOSPLIT,$0-8
	XORL CX, CX
	XGETBV
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·trampPtr(SB),NOSPLIT,$0-8
	LEAQ ·tramp(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·trampWaitPtr(SB),NOSPLIT,$0-8
	LEAQ ·trampWait(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·threadEntryPtr(SB),NOSPLIT,$0-8
	LEAQ ·threadEntry(SB), AX
	MOVQ AX, ret+0(FP)
	RET

TEXT ·cpuid7(SB),NOSPLIT,$0-4
	MOVL $7, AX
	XORL CX, CX
	CPUID
	MOVL BX, ret+0(FP)
	RET
