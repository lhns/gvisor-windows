// c/storage's overlay probe (supportsOverlay) against the Sentry's overlay on a host folder: lower2 holds
// subdir/file, a character device 0:0 (a whiteout) hides it from another layer, and RemoveAll(merged/subdir)
// must succeed (it failed with "unlinkat .../merged/subdir: directory not empty", docs/results/2r-step5-overlay.md).
// Three arrangements: the whiteout in lower1, in the upper layer, and made by the overlay itself (unlink).
// Usage: ovlsub BASE. Prints "FAIL ..." lines; exit 1 on any.
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <unistd.h>

static int fails;

static void fail(const char *what, const char *path) {
  printf("FAIL %s %s: %s\n", what, path, strerror(errno));
  fflush(stdout);
  fails++;
}

static void touch(const char *path) {
  int fd = open(path, O_WRONLY | O_CREAT, 0644);
  if (fd < 0) fail("create", path); else close(fd);
}

static void list(const char *dir) {
  DIR *d = opendir(dir);
  if (!d) { fail("opendir", dir); return; }
  struct dirent *e;
  printf("  %s:", dir);
  while ((e = readdir(d))) {
    if (strcmp(e->d_name, ".") && strcmp(e->d_name, "..")) printf(" %s(type %d)", e->d_name, e->d_type);
  }
  printf("\n");
  closedir(d);
}

// removeall is os.RemoveAll: list, unlink every entry, rmdir.
static int removeall(const char *dir) {
  DIR *d = opendir(dir);
  if (!d) return -1;
  struct dirent *e;
  char p[1200];
  int n = 0;
  while ((e = readdir(d))) {
    if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) continue;
    snprintf(p, sizeof p, "%s/%s", dir, e->d_name);
    if (unlink(p)) { int s = errno; closedir(d); errno = s; return -1; }
    n++;
  }
  closedir(d);
  printf("  removed %d entries from %s\n", n, dir);
  return rmdir(dir);
}

static void scenario(const char *base, const char *name, int whiteout_in) {
  char l1[600], l2[600], up[600], wk[600], mg[600], p[1200], opts[2400];
  char root[500];
  snprintf(root, sizeof root, "%s/%s", base, name);
  snprintf(l1, sizeof l1, "%s/lower1", root);
  snprintf(l2, sizeof l2, "%s/lower2", root);
  snprintf(up, sizeof up, "%s/upper", root);
  snprintf(wk, sizeof wk, "%s/work", root);
  snprintf(mg, sizeof mg, "%s/merged", root);
  mkdir(root, 0755); mkdir(l1, 0755); mkdir(l2, 0755); mkdir(up, 0755); mkdir(wk, 0755); mkdir(mg, 0755);
  snprintf(p, sizeof p, "%s/subdir", l2); mkdir(p, 0755);
  snprintf(p, sizeof p, "%s/subdir/file", l2); touch(p);
  if (whiteout_in) {
    const char *layer = whiteout_in == 1 ? l1 : up;
    snprintf(p, sizeof p, "%s/subdir", layer); mkdir(p, 0755);
    snprintf(p, sizeof p, "%s/subdir/file", layer);
    if (mknod(p, S_IFCHR | 0, makedev(0, 0))) fail("mknod whiteout", p);
    struct stat st;
    if (lstat(p, &st) || !S_ISCHR(st.st_mode) || st.st_rdev != 0) { printf("FAIL %s: whiteout stat mode %o rdev %lx\n", p, (unsigned)st.st_mode, (unsigned long)st.st_rdev); fails++; }
  } else {
    snprintf(p, sizeof p, "%s/other", l1); touch(p);
  }
  snprintf(opts, sizeof opts, "lowerdir=%s:%s,upperdir=%s,workdir=%s", l1, l2, up, wk);
  if (mount("overlay", mg, "overlay", 0, opts)) { printf("FAIL %s: mount overlay: %s\n", name, strerror(errno)); fails++; return; }
  printf("%s:\n", name);
  snprintf(p, sizeof p, "%s/subdir", mg);
  list(p);
  if (!whiteout_in) {
    // The overlay makes the whiteout itself.
    snprintf(p, sizeof p, "%s/subdir/file", mg);
    if (unlink(p)) fail("unlink merged file", p);
    snprintf(p, sizeof p, "%s/subdir", mg);
    list(p);
  }
  snprintf(p, sizeof p, "%s/subdir", mg);
  struct dirent *e;
  DIR *d = opendir(p);
  int left = 0;
  if (d) { while ((e = readdir(d))) if (strcmp(e->d_name, ".") && strcmp(e->d_name, "..")) left++; closedir(d); }
  if (left) { printf("FAIL %s: merged/subdir lists %d entries, want 0\n", name, left); fails++; }
  if (removeall(p)) fail("RemoveAll", p);
  if (access(p, F_OK) == 0) { printf("FAIL %s: merged/subdir still exists\n", name); fails++; }
  umount(mg);
}

int main(int argc, char **argv) {
  if (argc != 2) { fprintf(stderr, "usage: ovlsub BASE\n"); return 2; }
  mkdir(argv[1], 0755);
  umask(0);
  scenario(argv[1], "whiteout-in-lower1", 1);
  scenario(argv[1], "whiteout-in-upper", 2);
  scenario(argv[1], "whiteout-by-overlay", 0);
  printf("ovlsub: %d failures\n", fails);
  return fails ? 1 : 0;
}
