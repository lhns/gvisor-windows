#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static __thread int tv = 5;

__attribute__((noinline)) int work(const char *s) {
	char buf[64];
	strncpy(buf, s, 63);
	buf[63] = 0;
	return (int)strlen(buf) + tv;
}

int main(int argc, char **argv) {
	errno = 0;
	char *p = malloc(100000);
	char *q = malloc(1 << 20);
	memset(p, 'x', 100000);
	memset(q, 'y', 1 << 20);
	printf("hello from glibc: argc=%d tv=%d work=%d heap=%s\n", argc, tv, work("abcde"),
	       (p[99999] == 'x' && q[(1 << 20) - 1] == 'y') ? "ok" : "bad");
	fflush(stdout);
	free(p);
	free(q);
	return 7;
}
