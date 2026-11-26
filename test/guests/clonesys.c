/* clone through glibc's syscall() function: the `syscall` instruction sits behind register
 * moves, so the stub patches it with a veneer (the return address is inside the stub's
 * veneer region), unlike the `mov $56,%eax; syscall` of fork(). The child of a new address
 * space resumes at that return address. */
#define _GNU_SOURCE
#include <sched.h>
#include <signal.h>
#include <stdio.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

int main(int argc, char **argv) {
	(void)argv;
	long flags = SIGCHLD | (argc > 1 ? CLONE_NEWPID | CLONE_NEWUTS | CLONE_NEWIPC : 0);
	long p = syscall(SYS_clone, flags, 0, 0, 0, 0);
	if (p == 0) {
		write(1, "child\n", 6);
		_exit(3);
	}
	int st = 0;
	waitpid((pid_t)p, &st, 0);
	printf("parent: child exited %d\n", WEXITSTATUS(st));
	return 0;
}
