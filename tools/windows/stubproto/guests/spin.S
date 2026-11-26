# Pure computation for about a second: a target for interrupting a running thread.
.globl _start
.text
_start:
	mov $3000000000, %rcx
1:	dec %rcx
	jnz 1b
	xor %edi, %edi
	mov $231, %eax
	syscall
