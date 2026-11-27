// c/storage's native-diff probe (doesSupportNativeDiff, drivers/overlay/check.go) against the Sentry's overlay on a host
// folder: l2/d carries the xattr trusted.overlay.opaque=y as a plain xattr of a lower layer, a write into merged/d copies
// d up, and the upper's d must not carry the xattr ("opaque flag erroneously copied up", docs/results/2s-overlay-default.md).
// Usage: ovlopaque BASE. Prints "FAIL ..." lines; exit 1 on any.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/xattr.h>
#include <unistd.h>

static int fails;

static void fail(const char *what, const char *path) {
  printf("FAIL %s %s: %s\n", what, path, strerror(errno));
  fflush(stdout);
  fails++;
}

static void sub(char *out, size_t n, const char *base, const char *name) { snprintf(out, n, "%s/%s", base, name); }

static void scenario(const char *base, const char *xname) {
  char root[600], l1[700], l2[700], l3[700], wk[700], mg[700], p[800], opts[3000];
  snprintf(root, sizeof root, "%s/%s", base, xname[0] == 't' ? "trusted" : "user");
  mkdir(root, 0755);
  sub(l1, sizeof l1, root, "l1"); sub(l2, sizeof l2, root, "l2"); sub(l3, sizeof l3, root, "l3");
  sub(wk, sizeof wk, root, "work"); sub(mg, sizeof mg, root, "merged");
  mkdir(l1, 0755); mkdir(l2, 0755); mkdir(l3, 0755); mkdir(wk, 0755); mkdir(mg, 0755);
  sub(p, sizeof p, l1, "d"); mkdir(p, 0755);
  sub(p, sizeof p, l1, "d1"); mkdir(p, 0755);
  sub(p, sizeof p, l2, "d"); mkdir(p, 0755);
  if (lsetxattr(p, xname, "y", 1, 0)) { fail("lsetxattr", p); return; }
  snprintf(opts, sizeof opts, "lowerdir=%s:%s,upperdir=%s,workdir=%s%s", l2, l1, l3, wk, xname[0] == 'u' ? ",userxattr" : "");
  if (mount("overlay", mg, "overlay", 0, opts)) { printf("FAIL %s: mount overlay: %s\n", xname, strerror(errno)); fails++; return; }
  sub(p, sizeof p, mg, "d/f");
  int fd = open(p, O_WRONLY | O_CREAT, 0644);
  if (fd < 0) fail("create in merged/d", p); else close(fd);
  sub(p, sizeof p, l3, "d");
  char v[16];
  ssize_t n = lgetxattr(p, xname, v, sizeof v);
  if (n >= 0) { printf("FAIL %s: opaque flag erroneously copied up (value %.*s)\n", xname, (int)n, v); fails++; }
  else if (errno != ENODATA) fail("lgetxattr upper", p);
  else printf("%s: not copied up\n", xname);
  umount(mg);
}

int main(int argc, char **argv) {
  if (argc != 2) { fprintf(stderr, "usage: ovlopaque BASE\n"); return 2; }
  mkdir(argv[1], 0755);
  umask(0);
  scenario(argv[1], "trusted.overlay.opaque");
  printf("ovlopaque: %d failures\n", fails);
  return fails ? 1 : 0;
}
