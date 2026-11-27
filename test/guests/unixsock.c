// Reproducer: a SOCK_SEQPACKET unix socket bound on a given directory, connected to directly
// and through an O_PATH descriptor of the socket file (/proc/self/fd/N), as Podman does for
// a container's attach socket. Usage: unixsock DIR. Prints one line per connect.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>

static int try_connect(const char *path) {
  int c = socket(AF_UNIX, SOCK_SEQPACKET, 0);
  struct sockaddr_un a = {.sun_family = AF_UNIX};
  strncpy(a.sun_path, path, sizeof a.sun_path - 1);
  int r = connect(c, (struct sockaddr *)&a, sizeof a);
  int e = errno;
  close(c);
  return r == 0 ? 0 : e;
}

int main(int argc, char **argv) {
  char path[256], proc[64];
  snprintf(path, sizeof path, "%s/sock", argv[1]);
  unlink(path);
  int s = socket(AF_UNIX, SOCK_SEQPACKET, 0);
  struct sockaddr_un a = {.sun_family = AF_UNIX};
  strncpy(a.sun_path, path, sizeof a.sun_path - 1);
  if (bind(s, (struct sockaddr *)&a, sizeof a) || listen(s, 5)) { printf("bind/listen: %s\n", strerror(errno)); return 1; }
  printf("direct: %s\n", strerror(try_connect(path)));
  int p = open(path, O_PATH);
  snprintf(proc, sizeof proc, "/proc/self/fd/%d", p);
  printf("proc-fd: %s\n", strerror(try_connect(proc)));
  return 0;
}
