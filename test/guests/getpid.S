# 300000 getpid through one patched site, then "bench done".
.globl _start
.text
_start:
	mov $300000, %r12d
1:	mov $39, %eax
	syscall
	dec %r12d
	jnz 1b
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
