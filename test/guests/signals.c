/* Signal tests, one mode per run: signals MODE.  Each prints "MODE ok" on success. */
#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <setjmp.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <time.h>
#include <ucontext.h>
#include <unistd.h>

static volatile sig_atomic_t hits;
static sigjmp_buf jb;
static void *alt_lo, *alt_hi;
static volatile int on_alt;

static int fail(const char *m) {
	printf("FAIL: %s (errno %d)\n", m, errno);
	return 1;
}
static void act(int sig, void (*h)(int, siginfo_t *, void *), int flags) {
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_sigaction = h;
	sa.sa_flags = SA_SIGINFO | flags;
	sigemptyset(&sa.sa_mask);
	sigaction(sig, &sa, NULL);
}

static void count(int s, siginfo_t *i, void *c) {
	(void)s; (void)i; (void)c;
	hits++;
}

/* The handler runs before kill() returns and the vector registers survive it. */
static void fpuhog(int s, siginfo_t *i, void *c) {
	(void)s; (void)i; (void)c;
	volatile double a = 1.5;
	for (int k = 0; k < 100; k++) a = a * 1.0001 + k;
	hits++;
}
static int m_self(void) {
	act(SIGUSR1, fpuhog, 0);
	uint64_t bits = 0x400C000000000000ull, back = 0; /* 3.5 */
	long pid = getpid(), ret;
	asm volatile("movq %2, %%xmm5\n\tsyscall\n\tmovq %%xmm5, %1"
		     : "=a"(ret), "=r"(back)
		     : "r"(bits), "0"(62L), "D"(pid), "S"((long)SIGUSR1)
		     : "rcx", "r11", "xmm5", "memory");
	if (ret != 0 || hits != 1) return fail("handler not run before kill returned");
	if (back != bits) return fail("xmm5 clobbered by the handler");
	raise(SIGUSR1);
	if (hits != 2) return fail("raise");
	sigset_t b, old;
	sigemptyset(&b);
	sigaddset(&b, SIGUSR1);
	sigprocmask(SIG_BLOCK, &b, &old);
	kill(getpid(), SIGUSR1);
	if (hits != 2) return fail("delivered while blocked");
	sigset_t pend;
	sigpending(&pend);
	if (!sigismember(&pend, SIGUSR1)) return fail("not pending");
	sigprocmask(SIG_SETMASK, &old, NULL);
	if (hits != 3) return fail("not delivered on unblock");
	return 0;
}

static int m_alarm(void) {
	act(SIGALRM, count, 0);
	alarm(1);
	pause();
	if (hits != 1) return fail("alarm");
	struct itimerval it = {{0, 20000}, {0, 20000}};
	setitimer(ITIMER_REAL, &it, NULL);
	while (hits < 4) pause();
	memset(&it, 0, sizeof it);
	setitimer(ITIMER_REAL, &it, NULL);
	return 0;
}

static void segv(int s, siginfo_t *i, void *c) {
	(void)s; (void)c;
	hits++;
	siglongjmp(jb, i->si_addr == NULL ? 1 : (i->si_code == SEGV_ACCERR ? 2 : 3));
}
static int m_segv(void) {
	act(SIGSEGV, segv, SA_NODEFER);
	for (int k = 0; k < 3; k++) {
		int r = sigsetjmp(jb, 1);
		if (!r) *(volatile int *)0 = 1;
		else if (r != 1) return fail("null deref si_addr");
	}
	char *p = mmap(NULL, 4096, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	int r = sigsetjmp(jb, 1);
	if (!r) *(volatile char *)p = 1;
	else if (r != 2) return fail("PROT_NONE si_code");
	mprotect(p, 4096, PROT_READ | PROT_WRITE);
	*(volatile char *)p = 1;
	if (hits != 4) return fail("hit count");
	return 0;
}

static void onalt(int s, siginfo_t *i, void *c) {
	(void)s; (void)i; (void)c;
	char here;
	on_alt = (void *)&here >= alt_lo && (void *)&here < alt_hi;
	siglongjmp(jb, 1);
}
static int rec(int n) {
	volatile char pad[512];
	pad[0] = (char)n;
	return rec(n + 1) + pad[0];
}
static int m_altstack(void) {
	size_t sz = 64 * 1024;
	alt_lo = malloc(sz);
	alt_hi = (char *)alt_lo + sz;
	stack_t ss = {.ss_sp = alt_lo, .ss_size = sz, .ss_flags = 0};
	if (sigaltstack(&ss, NULL)) return fail("sigaltstack");
	act(SIGSEGV, onalt, SA_ONSTACK | SA_NODEFER);
	if (!sigsetjmp(jb, 1)) rec(0);
	if (!on_alt) return fail("handler not on the alternate stack");
	return 0;
}

/* sigreturn with a modified context: rax (the syscall result), r12 and rip. */
static void modctx(int s, siginfo_t *i, void *c) {
	(void)i;
	ucontext_t *uc = c;
	if (s == SIGSEGV) {
		uc->uc_mcontext.gregs[REG_RIP] += 6; /* skip movl $1,(%rax) */
		uc->uc_mcontext.gregs[REG_RAX] = 777;
	} else {
		uc->uc_mcontext.gregs[REG_RAX] = 1234;
		uc->uc_mcontext.gregs[REG_R12] = 99;
	}
}
static int m_ctx(void) {
	act(SIGUSR2, modctx, 0);
	act(SIGSEGV, modctx, SA_NODEFER);
	long pid = getpid(), ret;
	register long r12 asm("r12") = 5;
	asm volatile("syscall"
		     : "=a"(ret), "+r"(r12)
		     : "0"(62L), "D"(pid), "S"((long)SIGUSR2)
		     : "rcx", "r11", "memory");
	if (ret != 1234 || r12 != 99) return fail("modified rax/r12 not restored by sigreturn");
	long r;
	asm volatile("movl $1,(%%rax)" : "=a"(r) : "0"(0L) : "memory");
	if (r != 777) return fail("rip/rax modified in a SEGV handler");
	return 0;
}

/* A signal interrupts a thread blocked in a syscall. */
static pthread_t main_thr;
static int wfd = -1; /* if set, the kicker writes a byte after the second signal */
static void *kicker(void *a) {
	int sig = (int)(intptr_t)a;
	usleep(100000);
	pthread_kill(main_thr, sig);
	usleep(100000);
	pthread_kill(main_thr, sig);
	if (wfd >= 0) {
		usleep(100000);
		write(wfd, "x", 1);
	}
	return NULL;
}
static int futex_word;
static int m_eintr(void) {
	main_thr = pthread_self();
	pthread_t t;
	act(SIGUSR1, count, 0); /* no SA_RESTART */
	pthread_create(&t, NULL, kicker, (void *)(intptr_t)SIGUSR1);
	struct timespec req = {5, 0}, rem = {0, 0};
	int r = nanosleep(&req, &rem);
	if (r != -1 || errno != EINTR || hits != 1 || rem.tv_sec < 3) return fail("nanosleep not interrupted");
	long f = syscall(SYS_futex, &futex_word, 0 /*FUTEX_WAIT*/, 0, NULL, NULL, 0);
	if (f != -1 || errno != EINTR || hits != 2) return fail("futex wait not interrupted");
	pthread_join(t, NULL);
	/* SA_RESTART: read on a pipe continues after the handler and gets the data */
	hits = 0;
	act(SIGUSR2, count, SA_RESTART);
	int fd[2];
	pipe(fd);
	wfd = fd[1];
	pthread_create(&t, NULL, kicker, (void *)(intptr_t)SIGUSR2);
	char buf[2];
	int n = read(fd[0], buf, 1), e = errno;
	if (n != 1 || hits != 2) {
		printf("read n=%d errno=%d hits=%d
", n, e, (int)hits);
		return fail("restarted read");
	}
	pthread_join(t, NULL);
	return 0;
}

static volatile sig_atomic_t chld;
static volatile int chld_status;
static void onchld(int s, siginfo_t *i, void *c) {
	(void)s; (void)c;
	int st = 0;
	if (waitpid(i->si_pid, &st, WNOHANG) > 0) {
		chld_status = st;
		chld = 1;
	}
}
static void usr1exit(int s) {
	(void)s;
	_exit(5);
}
static int m_child(void) {
	act(SIGCHLD, onchld, SA_NOCLDSTOP);
	sigset_t b, old;
	sigemptyset(&b);
	sigaddset(&b, SIGCHLD);
	sigprocmask(SIG_BLOCK, &b, &old);
	pid_t p = fork();
	if (p == 0) _exit(4);
	while (!chld) sigsuspend(&old);
	sigprocmask(SIG_SETMASK, &old, NULL);
	if (!WIFEXITED(chld_status) || WEXITSTATUS(chld_status) != 4) return fail("SIGCHLD handler status");
	signal(SIGCHLD, SIG_DFL);
	/* parent kills child: default action, then a handler */
	int st;
	p = fork();
	if (p == 0) {
		for (;;) pause();
	}
	usleep(50000);
	kill(p, SIGTERM);
	waitpid(p, &st, 0);
	if (!WIFSIGNALED(st) || WTERMSIG(st) != SIGTERM) return fail("child not killed by SIGTERM");
	p = fork();
	if (p == 0) {
		signal(SIGUSR1, usr1exit);
		for (;;) pause();
	}
	usleep(100000);
	kill(p, SIGUSR1);
	waitpid(p, &st, 0);
	if (!WIFEXITED(st) || WEXITSTATUS(st) != 5) return fail("child handler exit");
	/* stop and continue */
	p = fork();
	if (p == 0) {
		for (;;) pause();
	}
	usleep(50000);
	kill(p, SIGSTOP);
	waitpid(p, &st, WUNTRACED);
	if (!WIFSTOPPED(st) || WSTOPSIG(st) != SIGSTOP) return fail("not stopped");
	kill(p, SIGCONT);
	waitpid(p, &st, WCONTINUED);
	if (!WIFCONTINUED(st)) return fail("not continued");
	kill(p, SIGKILL);
	waitpid(p, &st, 0);
	if (!WIFSIGNALED(st) || WTERMSIG(st) != SIGKILL) return fail("SIGKILL");
	return 0;
}

static int m_group(void) {
	int st;
	pid_t p = fork();
	if (p == 0) {
		setpgid(0, 0);
		if (fork() == 0) {
			for (;;) pause();
		}
		for (;;) pause();
	}
	usleep(2000000); /* a fork takes a few hundred ms */
	if (getpgid(p) != p) return fail("getpgid");
	if (getpgrp() == p) return fail("parent pgrp");
	kill(-p, SIGKILL);
	waitpid(p, &st, 0);
	if (!WIFSIGNALED(st) || WTERMSIG(st) != SIGKILL) return fail("group kill");
	/* the grandchild, reparented to us (pid 1 here), died with the group */
	if (waitpid(-1, &st, 0) <= 0 || !WIFSIGNALED(st) || WTERMSIG(st) != SIGKILL) return fail("grandchild");
	if (kill(-p, 0) != -1 || errno != ESRCH) return fail("group still has members");
	return 0;
}

int main(int argc, char **argv) {
	static const struct {
		const char *n;
		int (*f)(void);
	} m[] = {{"self", m_self},   {"alarm", m_alarm}, {"segv", m_segv},   {"altstack", m_altstack},
		 {"ctx", m_ctx},     {"eintr", m_eintr}, {"child", m_child}, {"group", m_group}};
	for (unsigned i = 0; argc > 1 && i < sizeof m / sizeof m[0]; i++) {
		if (strcmp(argv[1], m[i].n)) continue;
		int r = m[i].f();
		if (!r) printf("%s ok\n", m[i].n);
		return r;
	}
	printf("usage\n");
	return 2;
}
