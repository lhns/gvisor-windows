// A SOCK_SEQPACKET unix socket bound and listening in a directory, as conmon's attach socket:
// bound through /proc/self/fd/DIRFD/sock (the path would be too long for sun_path), connected to
// by a child process directly, through an O_PATH descriptor of the socket file
// (/proc/self/fd/N, as Podman does), and from the parent too. Data is exchanged on each
// connection. Usage: unixsock DIR. Prints "name: Success" or the error per step; exit 1 on a failure.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <unistd.h>

static int fails;

static int connect_to(const char *path) {
  int c = socket(AF_UNIX, SOCK_SEQPACKET, 0);
  struct sockaddr_un a = {.sun_family = AF_UNIX};
  strncpy(a.sun_path, path, sizeof a.sun_path - 1);
  if (connect(c, (struct sockaddr *)&a, sizeof a)) { int e = errno; close(c); errno = e; return -1; }
  return c;
}

static void report(const char *name, int ok) {
  printf("%s: %s\n", name, ok ? "Success" : strerror(errno));
  fflush(stdout);
  if (!ok) fails++;
}

int main(int argc, char **argv) {
  char path[256], proc[64];
  snprintf(path, sizeof path, "%s/sock", argv[1]);
  unlink(path);
  int dirfd = open(argv[1], O_PATH | O_DIRECTORY);
  snprintf(proc, sizeof proc, "/proc/self/fd/%d/sock", dirfd);
  int s = socket(AF_UNIX, SOCK_SEQPACKET | SOCK_NONBLOCK, 0);
  struct sockaddr_un a = {.sun_family = AF_UNIX};
  strncpy(a.sun_path, proc, sizeof a.sun_path - 1);
  if (bind(s, (struct sockaddr *)&a, sizeof a) || chmod(path, 0700) || listen(s, 5)) { report("bind+listen", 0); return 1; }
  report("bind+listen", 1);

  pid_t pid = fork();
  if (pid == 0) {
    int c = connect_to(path);
    printf("child direct: %s\n", c >= 0 ? "Success" : strerror(errno));
    if (c >= 0) write(c, "direct", 6);
    int p = open(path, O_PATH);
    char pp[64];
    snprintf(pp, sizeof pp, "/proc/self/fd/%d", p);
    c = connect_to(pp);
    printf("child proc-fd: %s\n", c >= 0 ? "Success" : strerror(errno));
    if (c >= 0) write(c, "procfd", 6);
    fflush(stdout);
    _exit(0);
  }
  waitpid(pid, NULL, 0);
  int got = 0;
  for (int i = 0; i < 2; i++) {
    int c = accept(s, NULL, NULL);
    if (c < 0) break;
    char buf[16] = {0};
    if (read(c, buf, sizeof buf - 1) > 0) got++;
    close(c);
  }
  report("accepted and read from the child", got == 2);
  int c = connect_to(path);
  report("parent direct", c >= 0);
  return fails ? 1 : 0;
}
