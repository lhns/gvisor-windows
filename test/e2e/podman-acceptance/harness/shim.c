// LD_PRELOAD probe for netavark: make setsockopt(SOL_NETLINK, ...) succeed when gVisor answers
// ENOPROTOOPT, to find the next gap behind NETLINK_DUMP_STRICT_CHK. Not a fix.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <sys/socket.h>
int setsockopt(int fd, int level, int name, const void *val, socklen_t len) {
  static int (*real)(int, int, int, const void *, socklen_t);
  if (!real) real = dlsym(RTLD_NEXT, "setsockopt");
  int r = real(fd, level, name, val, len);
  if (r < 0 && errno == ENOPROTOOPT && level == 270 /* SOL_NETLINK */) return 0;
  return r;
}
