# 100000 getpid through a patched site, 2000 getppid through a site that cannot be
# patched (it sits between direct jumps, so no displaced window exists): ud2 path.
.globl _start
.text
_start:
	mov $100000, %r12d
1:	mov $39, %eax
	syscall
	dec %r12d
	jnz 1b
	mov $2000, %r12d
	mov $110, %ebx
2:	mov %ebx, %eax
	jmp 4f
4:	syscall
	jmp 5f
5:	dec %r12d
	jnz 2b
	mov $1, %edi
	lea msg(%rip), %rsi
	mov $11, %edx
	mov $1, %eax
	syscall
	xor %edi, %edi
	mov $231, %eax
	syscall
.data
msg: .ascii "bench done\n"
