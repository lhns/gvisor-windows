#include <stdio.h>
#include <sys/wait.h>
#include <unistd.h>

int main(void) {
	pid_t pid = fork();
	if (pid == 0) {
		write(1, "child\n", 6);
		_exit(3);
	}
	int st = 0;
	waitpid(pid, &st, 0);
	printf("parent: child exited %d\n", WEXITSTATUS(st));
	return 0;
}
