#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/auxv.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

static const char *auxv_check(int exec_level) {
	if (getauxval(AT_PAGESZ) != 4096) return "pagesz";
	if (!getauxval(AT_RANDOM)) return "random";
	if (!getauxval(AT_PHDR) || !getauxval(AT_PHNUM) || getauxval(AT_PHENT) != 56) return "phdr";
	if (getauxval(AT_UID) != 0 || getauxval(AT_EUID) != 0 || getauxval(AT_SECURE) != 0) return "ids";
	if (!getauxval(AT_SYSINFO_EHDR)) return "vdso";
	if (getauxval(AT_CLKTCK) != 100) return "clktck";
	const char *plat = (const char *)getauxval(AT_PLATFORM);
	if (!plat || strcmp(plat, "x86_64")) return "platform";
	const char *fn = (const char *)getauxval(AT_EXECFN);
	if (!fn || (exec_level && strcmp(fn, "/bin/exec"))) return "execfn";
	return "ok";
}


int main(int argc, char **argv) {
	if (argc >= 3 && !strcmp(argv[1], "chain")) {
		int n = atoi(argv[2]);
		const char *foo = getenv("FOO");
		printf("level %d: argc=%d argv0=%s FOO=%s auxv=%s\n", n, argc, argv[0], foo ? foo : "(unset)",
		       auxv_check(n > 1));
		fflush(stdout);
		if (n >= 3) return 0;
		char next[16], foo2[16];
		snprintf(next, sizeof next, "%d", n + 1);
		snprintf(foo2, sizeof foo2, "FOO=lvl%d", n + 1);
		char *av[] = {n == 1 ? "renamed" : argv[0], "chain", next, NULL};
		char *ev[] = {foo2, "PATH=/bin", NULL};
		execve("/bin/exec", av, ev);
		printf("execve failed: %d\n", errno);
		return 1;
	}
	if (argc >= 2 && !strcmp(argv[1], "child")) {
		printf("child: argc=%d a2=%s a3=%s X=%s auxv=%s\n", argc, argv[2], argv[3], getenv("X"), auxv_check(1));
		fflush(stdout);
		_exit(9);
	}
	if (argc >= 2 && !strcmp(argv[1], "forkexec")) {
		int st = 0;
		pid_t p = fork();
		if (p == 0) {
			char *av[] = {"exec", "child", "a b", "c", NULL};
			char *ev[] = {"X=1", NULL};
			execve("/bin/exec", av, ev);
			_exit(99);
		}
		pid_t w = waitpid(p, &st, 0);
		printf("forkexec: waited=%d exited=%d status=%d\n", w == p, WIFEXITED(st), WEXITSTATUS(st));
		p = fork();
		if (p == 0) {
			char *av[] = {"nonexistent", NULL};
			execve("/nonexistent", av, environ);
			_exit(errno == ENOENT ? 11 : 12);
		}
		waitpid(p, &st, 0);
		printf("forkexec: enoent child status=%d\n", WEXITSTATUS(st));
		/* three children at once, reaped by wait() */
		int sum = 0;
		for (int i = 1; i <= 3; i++)
			if (fork() == 0) _exit(i);
		for (int i = 0; i < 3; i++) {
			pid_t c = wait(&st);
			if (c > 0 && WIFEXITED(st)) sum += WEXITSTATUS(st);
		}
		int w2 = wait(&st), e2 = errno == ECHILD;
		printf("forkexec: sum=%d wait=%d errno_echild=%d\n", sum, w2, e2);
		return 0;
	}
	printf("usage\n");
	return 2;
}
