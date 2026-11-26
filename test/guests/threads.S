# Thread B counts forever. Thread A touches fresh anonymous pages (debug events) and
# then sleeps; the controller samples the counter while an event is held and while
# a sleep is served over shared memory.
.globl _start
.text
_start:
	lea counter(%rip), %rdi
	mov $9999, %eax
	syscall
	xor %edi, %edi
	mov $0x100000, %esi
	mov $3, %edx
	mov $0x22, %r10d
	mov $-1, %r8
	xor %r9d, %r9d
	mov $9, %eax
	syscall
	mov %rax, %rbx
	mov $0x50f00, %edi
	lea stack+16384(%rip), %rsi
	xor %edx, %edx
	xor %r10d, %r10d
	xor %r8d, %r8d
	mov $56, %eax
	syscall
	test %eax, %eax
	jz child
	mov $24, %r12d
1:	movb $1, (%rbx)
	add $32768, %rbx
	dec %r12d
	jnz 1b
	lea ts(%rip), %rdi
	xor %esi, %esi
	mov $35, %eax
	syscall
	mov $1, %edi
	lea msg(%rip), %rsi
	mov $13, %edx
	mov $1, %eax
	syscall
	xor %edi, %edi
	mov $231, %eax
	syscall
child:
	lea counter(%rip), %rax
2:	incq (%rax)
	jmp 2b
.data
ts: .quad 0, 100000000
msg: .ascii "threads done\n"
.bss
.lcomm counter, 8
.lcomm stack, 16384
