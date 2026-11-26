#include "textflag.h"

// Position-independent stub code. The Go linker places these routines in the
// controller binary; the controller copies their bytes (up to an 8-byte ud2
// sentinel) into the guest address space. They reference nothing but %gs:0x28
// (TEB.ArbitraryUserPointer = per-thread shared page) and the page itself.
// Page layout: see stubasm.go. Registers rax, rcx, r11 are free at a syscall
// site (the syscall instruction clobbers them); everything else is saved.

// Wait for state==2 (spin, then NtDelayExecution), then restore the guest
// registers from the page, set the %fs base and return to the guest.
// Expects R11 = page, AX = spin budget.
#define WAITRET \
wait: \
	CMPL 0(R11), $2 \
	JEQ  done \
	PAUSE \
	DECQ AX \
	JNZ  wait \
	MOVQ 0x38(R11), SP \
	MOVQ 0x30(R11), AX \
	XORL CX, CX \
	LEAQ 0x48(R11), DX \
	CALL AX \
	BYTE $0x65; BYTE $0x4C; BYTE $0x8B; BYTE $0x1C; BYTE $0x25; LONG $0x28 \
	MOVQ 0x40(R11), AX \
	JMP  wait \
done: \
	MOVL $0, 0(R11) \
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
	JMP  CX

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
	MOVQ CX, 0x18(R11)
	MOVL $1, 0(R11)
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
