/* Sleeps, clocks, timerfd, POSIX timers, interval timers and timeouts: clocks [GROUP...].
 * Without a group every group runs. Each check prints its measurement; a failed check is
 * reported and the run goes on, the exit code is 1 if any failed. Prints "clocks ok" at the end.
 * Tolerances are generous (CI runners are noisy): a wait of T ms returns within [T-1, T+50] ms. */
#define _GNU_SOURCE
#include <errno.h>
#include <poll.h>
#include <pthread.h>
#include <sched.h>
#include <semaphore.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/epoll.h>
#include <sys/select.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <sys/timerfd.h>
#include <time.h>
#include <unistd.h>

#define SLACK_HI 50.0 /* ms a wait may overshoot */
#define SLACK_LO 1.0  /* ms a wait may undershoot */

static int failed;

static void bad(const char *what, const char *fmt, ...) __attribute__((format(printf, 2, 3)));
static void bad(const char *what, const char *fmt, ...) {
	char b[256];
	int e = errno;
	va_list ap;
	va_start(ap, fmt);
	vsnprintf(b, sizeof b, fmt, ap);
	va_end(ap);
	printf("FAIL %s: %s (errno %d)\n", what, b, e);
	failed++;
}

static double now_ms(clockid_t c) {
	struct timespec t;
	clock_gettime(c, &t);
	return t.tv_sec * 1e3 + t.tv_nsec / 1e6;
}
static struct timespec ms_ts(double ms) {
	struct timespec t;
	t.tv_sec = (time_t)(ms / 1000);
	t.tv_nsec = (long)((ms - t.tv_sec * 1000.0) * 1e6);
	return t;
}
/* The time of clock c plus ms (negative for the past). */
static struct timespec ts_add(clockid_t c, double ms) {
	struct timespec t;
	clock_gettime(c, &t);
	int64_t n = (int64_t)t.tv_sec * 1000000000 + t.tv_nsec + (int64_t)(ms * 1e6);
	t.tv_sec = n / 1000000000;
	t.tv_nsec = n % 1000000000;
	return t;
}
/* Checks that got (ms) is within [want - SLACK_LO, want + hi]. */
static void within(const char *what, double got, double want, double hi) {
	printf("  %-36s %8.2f ms (want %g)\n", what, got, want);
	if (got < want - SLACK_LO || got > want + hi) bad(what, "%.2f ms outside [%g, %g]", got, want - SLACK_LO, want + hi);
}
#define WITHIN(what, got, want) within(what, got, want, SLACK_HI)

static volatile sig_atomic_t alarms, vtalarms, proftalarms, usr1s;
static void on_alrm(int s) { (void)s; alarms++; }
static void on_vt(int s) { (void)s; vtalarms++; }
static void on_prof(int s) { (void)s; proftalarms++; }
static void on_usr1(int s) { (void)s; usr1s++; }
static void handler(int sig, void (*h)(int)) {
	struct sigaction sa;
	memset(&sa, 0, sizeof sa);
	sa.sa_handler = h;
	sigemptyset(&sa.sa_mask);
	sigaction(sig, &sa, NULL);
}

static void t_sleep(void) {
	puts("sleep:");
	struct timespec r = ms_ts(10), rem;
	double t0 = now_ms(CLOCK_MONOTONIC);
	if (nanosleep(&r, &rem)) bad("nanosleep", "failed");
	WITHIN("nanosleep 10ms", now_ms(CLOCK_MONOTONIC) - t0, 10);

	r = ms_ts(1);
	t0 = now_ms(CLOCK_MONOTONIC);
	nanosleep(&r, NULL);
	WITHIN("nanosleep 1ms", now_ms(CLOCK_MONOTONIC) - t0, 1);

	r = ms_ts(100);
	t0 = now_ms(CLOCK_MONOTONIC);
	nanosleep(&r, NULL);
	WITHIN("nanosleep 100ms", now_ms(CLOCK_MONOTONIC) - t0, 100);

	t0 = now_ms(CLOCK_MONOTONIC);
	usleep(20000);
	WITHIN("usleep 20ms", now_ms(CLOCK_MONOTONIC) - t0, 20);

	t0 = now_ms(CLOCK_MONOTONIC);
	if (sleep(0) != 0) bad("sleep(0)", "returned nonzero");
	if (now_ms(CLOCK_MONOTONIC) - t0 > 20) bad("sleep(0)", "slow");

	clockid_t ids[] = {CLOCK_MONOTONIC, CLOCK_REALTIME, CLOCK_BOOTTIME};
	const char *names[] = {"MONOTONIC", "REALTIME", "BOOTTIME"};
	for (int i = 0; i < 3; i++) {
		char w[64];
		r = ms_ts(10);
		t0 = now_ms(CLOCK_MONOTONIC);
		int e = clock_nanosleep(ids[i], 0, &r, NULL);
		if (e) bad("clock_nanosleep", "%s relative: %d", names[i], e);
		snprintf(w, sizeof w, "clock_nanosleep rel %s 10ms", names[i]);
		WITHIN(w, now_ms(CLOCK_MONOTONIC) - t0, 10);

		struct timespec abs = ts_add(ids[i], 30);
		t0 = now_ms(CLOCK_MONOTONIC);
		e = clock_nanosleep(ids[i], TIMER_ABSTIME, &abs, NULL);
		if (e) bad("clock_nanosleep", "%s absolute: %d", names[i], e);
		snprintf(w, sizeof w, "clock_nanosleep abs %s +30ms", names[i]);
		WITHIN(w, now_ms(CLOCK_MONOTONIC) - t0, 30);

		abs = ts_add(ids[i], -500);
		t0 = now_ms(CLOCK_MONOTONIC);
		e = clock_nanosleep(ids[i], TIMER_ABSTIME, &abs, NULL);
		snprintf(w, sizeof w, "clock_nanosleep abs %s past", names[i]);
		if (e) bad(w, "returned %d", e);
		if (now_ms(CLOCK_MONOTONIC) - t0 > 30) bad(w, "did not return at once");
	}

	/* A timer signal interrupts the sleep: EINTR and the remaining time. */
	handler(SIGALRM, on_alrm);
	alarms = 0;
	struct itimerval it = {{0, 0}, {0, 20000}};
	setitimer(ITIMER_REAL, &it, NULL);
	r = ms_ts(2000);
	t0 = now_ms(CLOCK_MONOTONIC);
	int rc = nanosleep(&r, &rem);
	int e = errno;
	double el = now_ms(CLOCK_MONOTONIC) - t0, left = rem.tv_sec * 1e3 + rem.tv_nsec / 1e6;
	if (rc != -1 || e != EINTR) bad("nanosleep EINTR", "rc %d errno %d", rc, e);
	WITHIN("interrupted nanosleep elapsed", el, 20);
	printf("  remaining %.1f ms\n", left);
	if (left < 1900 || left > 2000 - 19) bad("nanosleep remaining", "%.1f ms", left);
}

static void t_clocks(void) {
	puts("clocks:");
	clockid_t ids[] = {CLOCK_MONOTONIC, CLOCK_REALTIME, CLOCK_BOOTTIME, CLOCK_MONOTONIC_RAW, CLOCK_MONOTONIC_COARSE, CLOCK_REALTIME_COARSE,
			   CLOCK_PROCESS_CPUTIME_ID, CLOCK_THREAD_CPUTIME_ID};
	const char *names[] = {"MONOTONIC", "REALTIME", "BOOTTIME", "MONOTONIC_RAW", "MONOTONIC_COARSE", "REALTIME_COARSE", "PROCESS_CPUTIME", "THREAD_CPUTIME"};
	for (int i = 0; i < 8; i++) {
		struct timespec p, q, res;
		if (clock_gettime(ids[i], &p)) { bad("clock_gettime", "%s", names[i]); continue; }
		int back = 0;
		for (int k = 0; k < 20000; k++) {
			clock_gettime(ids[i], &q);
			if (q.tv_sec < p.tv_sec || (q.tv_sec == p.tv_sec && q.tv_nsec < p.tv_nsec)) back++;
			p = q;
		}
		if (back) bad("monotonic", "%s went backwards %d times", names[i], back);
		memset(&res, 0, sizeof res);
		if (clock_getres(ids[i], &res) || res.tv_sec != 0 || res.tv_nsec <= 0 || res.tv_nsec > 20000000)
			bad("clock_getres", "%s: %ld.%09ld", names[i], (long)res.tv_sec, res.tv_nsec);
		printf("  %-18s ok, res %ld ns\n", names[i], res.tv_nsec);
	}

	/* The raw syscall agrees with the C library (which may use the vDSO). */
	struct timespec a, b;
	clock_gettime(CLOCK_MONOTONIC, &a);
	if (syscall(SYS_clock_gettime, CLOCK_MONOTONIC, &b)) bad("raw clock_gettime", "failed");
	double d = (b.tv_sec - a.tv_sec) * 1e3 + (b.tv_nsec - a.tv_nsec) / 1e6;
	if (d < -1 || d > 30) bad("raw vs libc MONOTONIC", "%.2f ms apart", d);

	/* REALTIME is a plausible date and agrees with time(), gettimeofday() and the syscalls. */
	clock_gettime(CLOCK_REALTIME, &a);
	if (a.tv_sec < 1767225600L /* 2026-01-01 */ || a.tv_sec > 4102444800L /* 2100 */) bad("REALTIME", "implausible %ld", (long)a.tv_sec);
	struct timeval tv;
	gettimeofday(&tv, NULL);
	time_t t = time(NULL);
	if (labs((long)(t - a.tv_sec)) > 2 || labs((long)(tv.tv_sec - a.tv_sec)) > 2) bad("time()/gettimeofday()", "%ld %ld vs %ld", (long)t, (long)tv.tv_sec, (long)a.tv_sec);
	if (syscall(SYS_gettimeofday, &tv, NULL) || labs((long)(tv.tv_sec - a.tv_sec)) > 2) bad("raw gettimeofday", "%ld", (long)tv.tv_sec);
	long rt = syscall(SYS_time, NULL);
	if (rt < 0 || labs(rt - (long)a.tv_sec) > 2) bad("raw time", "%ld vs %ld", rt, (long)a.tv_sec);

	/* REALTIME, MONOTONIC and BOOTTIME advance together across a sleep. */
	double m0 = now_ms(CLOCK_MONOTONIC), r0 = now_ms(CLOCK_REALTIME), b0 = now_ms(CLOCK_BOOTTIME), c0 = now_ms(CLOCK_MONOTONIC_RAW);
	usleep(100000);
	double dm = now_ms(CLOCK_MONOTONIC) - m0, dr = now_ms(CLOCK_REALTIME) - r0, db = now_ms(CLOCK_BOOTTIME) - b0, dc = now_ms(CLOCK_MONOTONIC_RAW) - c0;
	printf("  across 100ms sleep: mono %.2f real %.2f boot %.2f raw %.2f\n", dm, dr, db, dc);
	WITHIN("MONOTONIC across sleep", dm, 100);
	if (dr < dm - 30 || dr > dm + 30) bad("REALTIME vs MONOTONIC", "%.2f vs %.2f", dr, dm);
	if (db < dm - 30 || db > dm + 30) bad("BOOTTIME vs MONOTONIC", "%.2f vs %.2f", db, dm);
	if (dc < dm - 30 || dc > dm + 30) bad("MONOTONIC_RAW vs MONOTONIC", "%.2f vs %.2f", dc, dm);
	double bm = now_ms(CLOCK_BOOTTIME) - now_ms(CLOCK_MONOTONIC);
	if (bm < -5) bad("BOOTTIME < MONOTONIC", "%.2f", bm);

	/* CPU-time clocks: advance while spinning, hardly while sleeping. */
	double p0 = now_ms(CLOCK_PROCESS_CPUTIME_ID), h0 = now_ms(CLOCK_THREAD_CPUTIME_ID);
	m0 = now_ms(CLOCK_MONOTONIC);
	volatile unsigned long sink = 0;
	while (now_ms(CLOCK_MONOTONIC) - m0 < 200) for (int k = 0; k < 1000; k++) sink += k;
	double dp = now_ms(CLOCK_PROCESS_CPUTIME_ID) - p0, dh = now_ms(CLOCK_THREAD_CPUTIME_ID) - h0;
	printf("  cputime after 200ms spin: process %.2f thread %.2f\n", dp, dh);
	if (dp < 40 || dp > 400) bad("PROCESS_CPUTIME spinning", "%.2f ms over 200 ms wall", dp);
	if (dh < 40 || dh > 400) bad("THREAD_CPUTIME spinning", "%.2f ms over 200 ms wall", dh);
	p0 = now_ms(CLOCK_PROCESS_CPUTIME_ID);
	h0 = now_ms(CLOCK_THREAD_CPUTIME_ID);
	usleep(200000);
	dp = now_ms(CLOCK_PROCESS_CPUTIME_ID) - p0;
	dh = now_ms(CLOCK_THREAD_CPUTIME_ID) - h0;
	printf("  cputime after 200ms sleep: process %.2f thread %.2f\n", dp, dh);
	if (dp > 100) bad("PROCESS_CPUTIME sleeping", "%.2f ms", dp);
	if (dh > 100) bad("THREAD_CPUTIME sleeping", "%.2f ms", dh);

	/* clock_getcpuclockid(0) is the process CPU clock. */
	clockid_t cc;
	if (clock_getcpuclockid(0, &cc) || clock_gettime(cc, &a)) bad("clock_getcpuclockid", "failed");

	/* clock_settime on MONOTONIC is refused; invalid ids and values too. */
	a = ms_ts(1);
	if (clock_settime(CLOCK_MONOTONIC, &a) == 0 || errno != EINVAL) bad("clock_settime MONOTONIC", "not EINVAL");
	if (clock_gettime((clockid_t)77, &a) == 0 || errno != EINVAL) bad("clock_gettime(77)", "not EINVAL");
	a.tv_sec = 0;
	a.tv_nsec = 2000000000L;
	if (nanosleep(&a, NULL) == 0 || errno != EINVAL) bad("nanosleep bad nsec", "not EINVAL");
}

/* Reads the 8-byte count; returns the ms it took, n = 0 if the read failed. */
static double fd_read(int fd, uint64_t *n) {
	double t0 = now_ms(CLOCK_MONOTONIC);
	*n = 0;
	if (read(fd, n, 8) != 8) *n = 0;
	return now_ms(CLOCK_MONOTONIC) - t0;
}

static void t_timerfd(void) {
	puts("timerfd:");
	uint64_t n;
	struct itimerspec its, cur;

	int fd = timerfd_create(CLOCK_MONOTONIC, 0);
	if (fd < 0) { bad("timerfd_create", "failed"); return; }
	memset(&its, 0, sizeof its);
	its.it_value = ms_ts(20);
	timerfd_settime(fd, 0, &its, NULL);
	timerfd_gettime(fd, &cur);
	double left = cur.it_value.tv_sec * 1e3 + cur.it_value.tv_nsec / 1e6;
	if (left <= 0 || left > 20) bad("timerfd_gettime", "remaining %.2f", left);
	/* The read starts a little after the gettime: it waits up to what was left. */
	within("timerfd relative 20ms read", fd_read(fd, &n), left - 4, SLACK_HI);
	if (n != 1) bad("timerfd count", "%llu", (unsigned long long)n);
	timerfd_gettime(fd, &cur);
	if (cur.it_value.tv_sec || cur.it_value.tv_nsec) bad("timerfd one-shot", "still armed");
	close(fd);

	/* Nonblocking: EAGAIN before, the count after; disarming. */
	fd = timerfd_create(CLOCK_MONOTONIC, TFD_NONBLOCK);
	its.it_value = ms_ts(30);
	timerfd_settime(fd, 0, &its, NULL);
	if (read(fd, &n, 8) != -1 || errno != EAGAIN) bad("nonblocking read", "not EAGAIN");
	usleep(60000);
	if (read(fd, &n, 8) != 8 || n != 1) bad("nonblocking read after", "n=%llu", (unsigned long long)n);
	if (read(fd, &n, 8) != -1 || errno != EAGAIN) bad("second read", "not EAGAIN");
	its.it_value = ms_ts(30);
	timerfd_settime(fd, 0, &its, NULL);
	memset(&its, 0, sizeof its);
	timerfd_settime(fd, 0, &its, NULL);
	usleep(60000);
	if (read(fd, &n, 8) != -1 || errno != EAGAIN) bad("disarmed", "fired anyway");
	close(fd);

	/* Absolute, on MONOTONIC, REALTIME and BOOTTIME. */
	clockid_t ids[] = {CLOCK_MONOTONIC, CLOCK_REALTIME, CLOCK_BOOTTIME};
	const char *names[] = {"MONOTONIC", "REALTIME", "BOOTTIME"};
	for (int i = 0; i < 3; i++) {
		char w[64];
		fd = timerfd_create(ids[i], 0);
		memset(&its, 0, sizeof its);
		its.it_value = ts_add(ids[i], 30);
		timerfd_settime(fd, TFD_TIMER_ABSTIME, &its, NULL);
		snprintf(w, sizeof w, "timerfd absolute %s +30ms", names[i]);
		within(w, fd_read(fd, &n), 30 - 4, SLACK_HI);
		if (n != 1) bad(w, "count %llu", (unsigned long long)n);
		/* An absolute time in the past fires at once. */
		its.it_value = ts_add(ids[i], -1000);
		timerfd_settime(fd, TFD_TIMER_ABSTIME, &its, NULL);
		snprintf(w, sizeof w, "timerfd absolute %s past", names[i]);
		double el = fd_read(fd, &n);
		if (n != 1 || el > 30) bad(w, "count %llu after %.2f ms", (unsigned long long)n, el);
		close(fd);
	}

	/* Periodic: the count of expirations is read at once. */
	fd = timerfd_create(CLOCK_MONOTONIC, 0);
	memset(&its, 0, sizeof its);
	its.it_value = ms_ts(10);
	its.it_interval = ms_ts(10);
	double t0 = now_ms(CLOCK_MONOTONIC);
	timerfd_settime(fd, 0, &its, NULL);
	within("periodic first read", fd_read(fd, &n), 10 - 2, SLACK_HI);
	if (n < 1) bad("periodic first count", "%llu", (unsigned long long)n);
	usleep(100000);
	fd_read(fd, &n);
	double el = now_ms(CLOCK_MONOTONIC) - t0;
	printf("  periodic 10ms: count %llu after %.1f ms\n", (unsigned long long)n, el);
	if (n < 6 || n > (uint64_t)(el / 10) + 2) bad("periodic count", "%llu over %.1f ms", (unsigned long long)n, el);
	/* The next read waits for the next tick: within one interval (plus slack). */
	el = fd_read(fd, &n);
	printf("  periodic next read %.2f ms\n", el);
	if (n < 1 || el > 10 + SLACK_HI) bad("periodic next read", "%.2f ms count %llu", el, (unsigned long long)n);
	close(fd);

	/* epoll with a timerfd and a timeout; level-triggered until read. */
	fd = timerfd_create(CLOCK_MONOTONIC, TFD_NONBLOCK);
	int ep = epoll_create1(0);
	struct epoll_event ev = {.events = EPOLLIN, .data.fd = fd}, out[4];
	epoll_ctl(ep, EPOLL_CTL_ADD, fd, &ev);
	memset(&its, 0, sizeof its);
	its.it_value = ms_ts(25);
	timerfd_settime(fd, 0, &its, NULL);
	t0 = now_ms(CLOCK_MONOTONIC);
	int rc = epoll_wait(ep, out, 4, 2000);
	WITHIN("epoll_wait timerfd 25ms", now_ms(CLOCK_MONOTONIC) - t0, 25);
	if (rc != 1 || out[0].data.fd != fd || !(out[0].events & EPOLLIN)) bad("epoll_wait timerfd", "rc %d", rc);
	if (epoll_wait(ep, out, 4, 0) != 1) bad("epoll level-triggered", "not ready again before the read");
	if (read(fd, &n, 8) != 8) bad("epoll read", "failed");
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = epoll_wait(ep, out, 4, 30);
	WITHIN("epoll_wait nothing 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != 0) bad("epoll_wait after read", "rc %d", rc);
	/* Periodic timer with epoll: three ticks. */
	its.it_value = ms_ts(10);
	its.it_interval = ms_ts(10);
	timerfd_settime(fd, 0, &its, NULL);
	for (int k = 0; k < 3; k++) {
		rc = epoll_wait(ep, out, 4, 1000);
		if (rc != 1) bad("epoll periodic", "tick %d rc %d", k, rc);
		if (read(fd, &n, 8) != 8) bad("epoll periodic read", "tick %d", k);
	}
	close(ep);
	close(fd);

	/* poll and select on a timerfd. */
	fd = timerfd_create(CLOCK_MONOTONIC, 0);
	memset(&its, 0, sizeof its);
	its.it_value = ms_ts(20);
	timerfd_settime(fd, 0, &its, NULL);
	struct pollfd pf = {fd, POLLIN, 0};
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = poll(&pf, 1, 2000);
	WITHIN("poll timerfd 20ms", now_ms(CLOCK_MONOTONIC) - t0, 20);
	if (rc != 1 || !(pf.revents & POLLIN)) bad("poll timerfd", "rc %d", rc);
	if (read(fd, &n, 8) != 8) bad("poll read", "failed");
	timerfd_settime(fd, 0, &its, NULL);
	fd_set rs;
	FD_ZERO(&rs);
	FD_SET(fd, &rs);
	struct timeval tv = {2, 0};
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = select(fd + 1, &rs, NULL, NULL, &tv);
	WITHIN("select timerfd 20ms", now_ms(CLOCK_MONOTONIC) - t0, 20);
	if (rc != 1) bad("select timerfd", "rc %d", rc);
	close(fd);

	/* Rearming: a timer set for 5 s and then 20 ms fires at the new time. */
	fd = timerfd_create(CLOCK_MONOTONIC, 0);
	its.it_value = ms_ts(5000);
	timerfd_settime(fd, 0, &its, NULL);
	its.it_value = ms_ts(20);
	timerfd_settime(fd, 0, &its, NULL);
	within("timerfd rearmed 5s -> 20ms", fd_read(fd, &n), 20 - 2, SLACK_HI);
	close(fd);
}

static void t_ptimer(void) {
	puts("posix timers:");
	sigset_t blk, old;
	sigemptyset(&blk);
	sigaddset(&blk, SIGUSR1);
	sigprocmask(SIG_BLOCK, &blk, &old);

	timer_t tm;
	struct sigevent sev;
	memset(&sev, 0, sizeof sev);
	sev.sigev_notify = SIGEV_SIGNAL;
	sev.sigev_signo = SIGUSR1;
	sev.sigev_value.sival_int = 42;
	if (timer_create(CLOCK_MONOTONIC, &sev, &tm)) { bad("timer_create", "failed"); return; }
	struct itimerspec its = {{0, 0}, ms_ts(20)}, cur;
	double t0 = now_ms(CLOCK_MONOTONIC);
	timer_settime(tm, 0, &its, NULL);
	timer_gettime(tm, &cur);
	double left = cur.it_value.tv_sec * 1e3 + cur.it_value.tv_nsec / 1e6;
	if (left <= 0 || left > 20) bad("timer_gettime", "remaining %.2f", left);
	siginfo_t si;
	memset(&si, 0, sizeof si);
	struct timespec to = ms_ts(2000);
	int sg = sigtimedwait(&blk, &si, &to);
	WITHIN("timer_create one-shot 20ms", now_ms(CLOCK_MONOTONIC) - t0, 20);
	if (sg != SIGUSR1) bad("sigtimedwait", "got %d", sg);
	else {
		if (si.si_code != SI_TIMER) bad("si_code", "%d", si.si_code);
		if (si.si_value.sival_int != 42) bad("si_value", "%d", si.si_value.sival_int);
		if (si.si_overrun != 0) bad("si_overrun", "%d", si.si_overrun);
	}
	timer_gettime(tm, &cur);
	if (cur.it_value.tv_sec || cur.it_value.tv_nsec) bad("one-shot", "still armed");

	/* Periodic with the signal blocked: one pending signal, the rest overruns. */
	its.it_value = ms_ts(10);
	its.it_interval = ms_ts(10);
	t0 = now_ms(CLOCK_MONOTONIC);
	timer_settime(tm, 0, &its, NULL);
	usleep(120000);
	double el = now_ms(CLOCK_MONOTONIC) - t0;
	to = ms_ts(500);
	sg = sigtimedwait(&blk, &si, &to);
	int ov = timer_getoverrun(tm);
	printf("  periodic 10ms: overrun %d (getoverrun) %d (siginfo) after %.1f ms\n", ov, sg == SIGUSR1 ? si.si_overrun : -1, el);
	if (sg != SIGUSR1) bad("periodic signal", "got %d", sg);
	else if (si.si_overrun < 4 || si.si_overrun > (int)(el / 10) + 1) bad("si_overrun", "%d after %.1f ms", si.si_overrun, el);
	if (ov < 4 || ov > (int)(el / 10) + 1) bad("timer_getoverrun", "%d after %.1f ms", ov, el);
	/* Delivered again at the next tick, with a fresh overrun count. */
	to = ms_ts(500);
	sg = sigtimedwait(&blk, &si, &to);
	if (sg != SIGUSR1 || si.si_overrun > 5) bad("periodic next signal", "got %d overrun %d", sg, sg > 0 ? si.si_overrun : -1);
	memset(&its, 0, sizeof its);
	timer_settime(tm, 0, &its, NULL);
	to = ms_ts(40);
	while (sigtimedwait(&blk, &si, &to) > 0) {}
	to = ms_ts(40);
	if (sigtimedwait(&blk, &si, &to) > 0) bad("disarmed timer", "signal");
	timer_delete(tm);

	/* Absolute time on REALTIME with a handler that interrupts a sleep. */
	handler(SIGUSR1, on_usr1);
	sigprocmask(SIG_SETMASK, &old, NULL);
	usr1s = 0;
	if (timer_create(CLOCK_REALTIME, &sev, &tm)) bad("timer_create REALTIME", "failed");
	memset(&its, 0, sizeof its);
	its.it_value = ts_add(CLOCK_REALTIME, 30);
	t0 = now_ms(CLOCK_MONOTONIC);
	timer_settime(tm, TIMER_ABSTIME, &its, NULL);
	struct timespec nap = ms_ts(2000);
	nanosleep(&nap, NULL);
	WITHIN("REALTIME abs timer interrupts sleep", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (usr1s != 1) bad("handler count", "%d", usr1s);
	timer_delete(tm);

	/* SIGEV_NONE: no signal, the remaining time runs down. */
	sev.sigev_notify = SIGEV_NONE;
	if (timer_create(CLOCK_MONOTONIC, &sev, &tm)) bad("timer_create SIGEV_NONE", "failed");
	memset(&its, 0, sizeof its);
	its.it_value = ms_ts(500);
	timer_settime(tm, 0, &its, NULL);
	usleep(100000);
	timer_gettime(tm, &cur);
	left = cur.it_value.tv_sec * 1e3 + cur.it_value.tv_nsec / 1e6;
	printf("  SIGEV_NONE remaining after 100ms of 500: %.1f ms\n", left);
	if (left > 420 || left < 300) bad("SIGEV_NONE remaining", "%.1f ms", left);
	timer_delete(tm);

	/* A CPU-time timer fires after the process has spun. */
	sev.sigev_notify = SIGEV_SIGNAL;
	sev.sigev_signo = SIGUSR1;
	if (timer_create(CLOCK_PROCESS_CPUTIME_ID, &sev, &tm)) bad("timer_create CPUTIME", "failed");
	else {
		usr1s = 0;
		memset(&its, 0, sizeof its);
		its.it_value = ms_ts(30);
		timer_settime(tm, 0, &its, NULL);
		t0 = now_ms(CLOCK_MONOTONIC);
		volatile unsigned long sink = 0;
		while (!usr1s && now_ms(CLOCK_MONOTONIC) - t0 < 3000) for (int k = 0; k < 1000; k++) sink += k;
		within("CPUTIME timer 30ms of spinning", now_ms(CLOCK_MONOTONIC) - t0, 30, 400);
		if (!usr1s) bad("CPUTIME timer", "no signal");
		timer_delete(tm);
	}
}

static void t_itimer(void) {
	puts("setitimer/alarm:");
	handler(SIGALRM, on_alrm);
	handler(SIGVTALRM, on_vt);
	handler(SIGPROF, on_prof);
	struct itimerval it, cur;
	sigset_t none;
	sigemptyset(&none);

	alarms = 0;
	memset(&it, 0, sizeof it);
	it.it_value.tv_usec = 20000;
	double t0 = now_ms(CLOCK_MONOTONIC);
	setitimer(ITIMER_REAL, &it, NULL);
	getitimer(ITIMER_REAL, &cur);
	if (cur.it_value.tv_sec || cur.it_value.tv_usec <= 0 || cur.it_value.tv_usec > 20000) bad("getitimer", "%ld us", (long)cur.it_value.tv_usec);
	while (!alarms && now_ms(CLOCK_MONOTONIC) - t0 < 2000) sigsuspend(&none);
	WITHIN("ITIMER_REAL one-shot 20ms", now_ms(CLOCK_MONOTONIC) - t0, 20);
	getitimer(ITIMER_REAL, &cur);
	if (cur.it_value.tv_sec || cur.it_value.tv_usec) bad("one-shot", "still armed");

	/* Periodic 10 ms for 150 ms. */
	alarms = 0;
	it.it_value.tv_usec = 10000;
	it.it_interval.tv_usec = 10000;
	t0 = now_ms(CLOCK_MONOTONIC);
	setitimer(ITIMER_REAL, &it, NULL);
	while (now_ms(CLOCK_MONOTONIC) - t0 < 150) { struct timespec s = ms_ts(200); nanosleep(&s, NULL); }
	memset(&it, 0, sizeof it);
	struct itimerval prev;
	setitimer(ITIMER_REAL, &it, &prev);
	printf("  periodic 10ms over 150 ms: %d signals, previous interval %ld us\n", (int)alarms, (long)prev.it_interval.tv_usec);
	if (alarms < 5 || alarms > 16) bad("periodic ITIMER_REAL", "%d signals in 150 ms", (int)alarms);
	if (prev.it_interval.tv_usec != 10000) bad("old interval", "%ld", (long)prev.it_interval.tv_usec);

	/* alarm(): the previous remaining seconds are returned, 0 cancels. */
	alarms = 0;
	alarm(5);
	unsigned r = alarm(1);
	if (r < 4 || r > 5) bad("alarm remaining", "%u", r);
	t0 = now_ms(CLOCK_MONOTONIC);
	while (!alarms && now_ms(CLOCK_MONOTONIC) - t0 < 3000) sigsuspend(&none);
	within("alarm(1)", now_ms(CLOCK_MONOTONIC) - t0, 1000, 300);
	alarm(2);
	r = alarm(0);
	if (r != 2 && r != 1) bad("alarm(0) remaining", "%u", r);
	if (alarm(0) != 0) bad("alarm(0) again", "nonzero");

	/* ITIMER_VIRTUAL and ITIMER_PROF count CPU time of the process: spin. */
	vtalarms = proftalarms = 0;
	memset(&it, 0, sizeof it);
	it.it_value.tv_usec = 30000;
	setitimer(ITIMER_VIRTUAL, &it, NULL);
	t0 = now_ms(CLOCK_MONOTONIC);
	volatile unsigned long sink = 0;
	while (!vtalarms && now_ms(CLOCK_MONOTONIC) - t0 < 3000) for (int k = 0; k < 1000; k++) sink += k;
	within("ITIMER_VIRTUAL 30ms of spinning", now_ms(CLOCK_MONOTONIC) - t0, 30, 400);
	if (!vtalarms) bad("ITIMER_VIRTUAL", "no signal");
	setitimer(ITIMER_PROF, &it, NULL);
	t0 = now_ms(CLOCK_MONOTONIC);
	while (!proftalarms && now_ms(CLOCK_MONOTONIC) - t0 < 3000) for (int k = 0; k < 1000; k++) sink += k;
	within("ITIMER_PROF 30ms of spinning", now_ms(CLOCK_MONOTONIC) - t0, 30, 400);
	if (!proftalarms) bad("ITIMER_PROF", "no signal");
	/* While sleeping neither counts: no signal in 100 ms. */
	vtalarms = 0;
	it.it_value.tv_usec = 30000;
	setitimer(ITIMER_VIRTUAL, &it, NULL);
	usleep(100000);
	if (vtalarms) bad("ITIMER_VIRTUAL sleeping", "fired while the process slept");
	memset(&it, 0, sizeof it);
	setitimer(ITIMER_VIRTUAL, &it, NULL);
}

static void *idle_thread(void *a) { (void)a; return NULL; }

static void t_timeouts(void) {
	puts("timeouts:");
	int p[2];
	if (pipe(p)) { bad("pipe", "failed"); return; }
	double t0;
	int rc;

	/* poll */
	struct pollfd pf = {p[0], POLLIN, 0};
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = poll(&pf, 1, 30);
	WITHIN("poll 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != 0) bad("poll", "rc %d", rc);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = poll(&pf, 1, 0);
	if (rc != 0 || now_ms(CLOCK_MONOTONIC) - t0 > 20) bad("poll 0", "rc %d or slow", rc);
	t0 = now_ms(CLOCK_MONOTONIC);
	poll(NULL, 0, 20);
	WITHIN("poll no fds 20ms", now_ms(CLOCK_MONOTONIC) - t0, 20);
	struct timespec ts = ms_ts(30);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = ppoll(&pf, 1, &ts, NULL);
	WITHIN("ppoll 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != 0) bad("ppoll", "rc %d", rc);

	/* select, pselect */
	fd_set rs;
	FD_ZERO(&rs);
	FD_SET(p[0], &rs);
	struct timeval tv = {0, 30000};
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = select(p[0] + 1, &rs, NULL, NULL, &tv);
	WITHIN("select 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != 0) bad("select", "rc %d", rc);
	printf("  select left %ld us\n", (long)tv.tv_usec);
	if (tv.tv_sec != 0 || tv.tv_usec > 5000) bad("select timeout update", "%ld us left", (long)tv.tv_usec);
	FD_ZERO(&rs);
	FD_SET(p[0], &rs);
	ts = ms_ts(30);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = pselect(p[0] + 1, &rs, NULL, NULL, &ts, NULL);
	WITHIN("pselect 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != 0) bad("pselect", "rc %d", rc);
	tv = (struct timeval){0, 20000};
	t0 = now_ms(CLOCK_MONOTONIC);
	select(0, NULL, NULL, NULL, &tv);
	WITHIN("select no fds 20ms", now_ms(CLOCK_MONOTONIC) - t0, 20);

	/* Readable data ends the wait at once. */
	if (write(p[1], "x", 1) != 1) bad("write", "failed");
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = poll(&pf, 1, 2000);
	if (rc != 1 || now_ms(CLOCK_MONOTONIC) - t0 > 30) bad("poll ready", "rc %d or slow", rc);
	char c;
	if (read(p[0], &c, 1) != 1) bad("read", "failed");

	/* epoll_wait, epoll_pwait */
	int ep = epoll_create1(0);
	struct epoll_event ev = {.events = EPOLLIN, .data.fd = p[0]}, out[2];
	epoll_ctl(ep, EPOLL_CTL_ADD, p[0], &ev);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = epoll_wait(ep, out, 2, 30);
	WITHIN("epoll_wait 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != 0) bad("epoll_wait", "rc %d", rc);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = epoll_pwait(ep, out, 2, 30, NULL);
	WITHIN("epoll_pwait 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != 0) bad("epoll_pwait", "rc %d", rc);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = epoll_wait(ep, out, 2, 0);
	if (rc != 0 || now_ms(CLOCK_MONOTONIC) - t0 > 20) bad("epoll_wait 0", "rc %d or slow", rc);
	/* A timer signal after 30 ms ends a 2 s epoll_wait with EINTR. */
	handler(SIGALRM, on_alrm);
	alarms = 0;
	struct itimerval it = {{0, 0}, {0, 30000}};
	setitimer(ITIMER_REAL, &it, NULL);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = epoll_wait(ep, out, 2, 2000);
	int e = errno;
	WITHIN("epoll_wait interrupted by a timer", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != -1 || e != EINTR) bad("epoll_wait EINTR", "rc %d errno %d", rc, e);
	close(ep);

	/* sigtimedwait and the futex-based waits */
	sigset_t s;
	sigemptyset(&s);
	sigaddset(&s, SIGUSR2);
	sigprocmask(SIG_BLOCK, &s, NULL);
	ts = ms_ts(30);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = sigtimedwait(&s, NULL, &ts);
	e = errno;
	WITHIN("sigtimedwait 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != -1 || e != EAGAIN) bad("sigtimedwait", "rc %d errno %d", rc, e);

	sem_t sem;
	sem_init(&sem, 0, 0);
	struct timespec abs = ts_add(CLOCK_REALTIME, 30);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = sem_timedwait(&sem, &abs);
	e = errno;
	WITHIN("sem_timedwait 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	if (rc != -1 || e != ETIMEDOUT) bad("sem_timedwait", "rc %d errno %d", rc, e);

	pthread_mutex_t m = PTHREAD_MUTEX_INITIALIZER;
	pthread_cond_t cv;
	pthread_condattr_t ca;
	pthread_condattr_init(&ca);
	pthread_condattr_setclock(&ca, CLOCK_MONOTONIC);
	pthread_cond_init(&cv, &ca);
	abs = ts_add(CLOCK_MONOTONIC, 30);
	pthread_mutex_lock(&m);
	t0 = now_ms(CLOCK_MONOTONIC);
	rc = pthread_cond_timedwait(&cv, &m, &abs);
	WITHIN("pthread_cond_timedwait 30ms", now_ms(CLOCK_MONOTONIC) - t0, 30);
	pthread_mutex_unlock(&m);
	if (rc != ETIMEDOUT) bad("pthread_cond_timedwait", "rc %d", rc);

	pthread_t th;
	if (pthread_create(&th, NULL, idle_thread, NULL)) bad("pthread_create", "failed");
	ts = ms_ts(20);
	t0 = now_ms(CLOCK_MONOTONIC);
	nanosleep(&ts, NULL);
	WITHIN("nanosleep after pthread_create", now_ms(CLOCK_MONOTONIC) - t0, 20);
	pthread_join(th, NULL);
}

static const struct {
	const char *name;
	void (*fn)(void);
} groups[] = {
	{"sleep", t_sleep}, {"clocks", t_clocks}, {"timerfd", t_timerfd}, {"ptimer", t_ptimer}, {"itimer", t_itimer}, {"timeouts", t_timeouts},
};

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	for (unsigned g = 0; g < sizeof groups / sizeof *groups; g++) {
		int want = argc < 2;
		for (int i = 1; i < argc; i++)
			if (!strcmp(argv[i], groups[g].name)) want = 1;
		if (!want) continue;
		int before = failed;
		groups[g].fn();
		printf("%s: %s\n", groups[g].name, failed == before ? "ok" : "FAILED");
	}
	if (failed) {
		printf("clocks: %d checks failed\n", failed);
		return 1;
	}
	puts("clocks ok");
	return 0;
}
