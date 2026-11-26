#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

typedef long (*fn)(long, long);

static unsigned char *page(int prot) {
	return mmap(0, 4096, prot, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
}

int main(void) {
	long pid = getpid(), ppid = getppid();
	/* mov $39,%eax; syscall; ret   (getpid) */
	unsigned char getpid_code[] = {0xb8, 0x27, 0, 0, 0, 0x0f, 0x05, 0xc3};
	/* mov $110,%eax; syscall; ret  (getppid) */
	unsigned char getppid_code[] = {0xb8, 0x6e, 0, 0, 0, 0x0f, 0x05, 0xc3};
	/* mov %rdi,%rax; syscall; ret  (number from a register) */
	unsigned char reg_code[] = {0x48, 0x89, 0xf8, 0x0f, 0x05, 0xc3};

	/* 1: write code into an RW page, then make it executable */
	unsigned char *p = page(PROT_READ | PROT_WRITE);
	memcpy(p, getpid_code, sizeof getpid_code);
	mprotect(p, 4096, PROT_READ | PROT_EXEC);
	if (((fn)p)(0, 0) == pid) puts("jit1 ok"); else puts("jit1 FAIL");

	/* 2: run code in an RWX page, then overwrite it and run it again */
	unsigned char *q = page(PROT_READ | PROT_WRITE | PROT_EXEC);
	memcpy(q, getpid_code, sizeof getpid_code);
	long r1 = ((fn)q)(0, 0);
	memcpy(q, getppid_code, sizeof getppid_code);
	long r2 = ((fn)q)(0, 0);
	if (r1 == pid && r2 == ppid) puts("jit2 ok"); else puts("jit2 FAIL");

	/* 3: a register-number syscall in generated code, rewritten once */
	unsigned char *r = page(PROT_READ | PROT_WRITE | PROT_EXEC);
	memcpy(r, reg_code, sizeof reg_code);
	long a = ((fn)r)(39, 0);
	memcpy(r, reg_code, sizeof reg_code);
	long b = ((fn)r)(110, 0);
	if (a == pid && b == ppid) puts("jit3 ok"); else puts("jit3 FAIL");
	return 0;
}
