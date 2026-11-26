# Static asm guest, no libc, no %fs. One patchable write, one unpatchable (register
# number) write that must go through ud2.
.globl _start
.text
_start:
	mov $1, %edi
	lea msg1(%rip), %rsi
	mov $15, %edx
	mov $1, %eax
	syscall
	mov $1, %edi
	lea msg2(%rip), %rsi
	mov $12, %edx
	mov $1, %ebx
	mov %ebx, %eax
	syscall
	xor %edi, %edi
	mov $231, %eax
	syscall
.data
msg1: .ascii "hello from asm\n"
msg2: .ascii "ud2 path ok\n"
