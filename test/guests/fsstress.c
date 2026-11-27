// Stress of the create/lookup paths Podman, crun and conmon use, in parallel "containers": each worker
// unshares a mount namespace, mounts a tmpfs on a fixed directory of BASE (as the container directory),
// and loops: (A) writes userdata/config.json by temporary file + rename and opens it at once; (B) builds a
// rootfs/dev tmpfs, creates dev/null as a file and bind-mounts /dev/null on it, and opens it; (C) checks
// that the namespace's /ctr holds only its own files. Failures are printed as "FAIL tag worker iter: error".
// Usage: fsstress BASE WORKERS ITERATIONS. Exit 1 if any check failed.
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/sysmacros.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

static int worker, fails;
static const char *base;

static void fail(const char *tag, int iter, const char *what) {
  printf("FAIL %s worker %d iter %d: %s: %s\n", tag, worker, iter, what, strerror(errno));
  fflush(stdout);
  fails++;
}

static int writefile(const char *path, const char *s, int flags) {
  int fd = open(path, flags | O_WRONLY | O_CREAT, 0644);
  if (fd < 0) return -1;
  if (write(fd, s, strlen(s)) < 0) { close(fd); return -1; }
  return close(fd);
}

static void checkfile(const char *tag, int iter, const char *path, const char *want) {
  char buf[256] = {0};
  int fd = open(path, O_RDONLY);
  if (fd < 0) { fail(tag, iter, path); return; }
  ssize_t n = read(fd, buf, sizeof buf - 1);
  close(fd);
  if (n < 0 || strcmp(buf, want)) { errno = 0; fail(tag, iter, "wrong content"); }
}

static int listed(const char *dir, const char *name) {
  DIR *d = opendir(dir);
  if (!d) return 0;
  struct dirent *e;
  int found = 0;
  while ((e = readdir(d))) if (!strcmp(e->d_name, name)) found = 1;
  closedir(d);
  return found;
}

static void run(int iters) {
  char ctr[512], p[600], q[600], tok[64];
  snprintf(ctr, sizeof ctr, "%s/ctr", base);
  if (unshare(CLONE_NEWNS)) { fail("unshare", 0, "unshare"); return; }
  mount(NULL, "/", NULL, MS_REC | MS_PRIVATE, NULL);
  if (mount("tmpfs", ctr, "tmpfs", 0, "mode=755")) { fail("mount-ctr", 0, ctr); return; }
  for (int i = 0; i < iters; i++) {
    snprintf(tok, sizeof tok, "w%d-i%d", worker, i);
    // A: the container directory with userdata/config.json.
    snprintf(p, sizeof p, "%s/c%d", ctr, i);
    if (mkdir(p, 0755)) { fail("A:mkdir", i, p); continue; }
    snprintf(p, sizeof p, "%s/c%d/userdata", ctr, i);
    if (mkdir(p, 0755)) { fail("A:mkdir-userdata", i, p); continue; }
    snprintf(p, sizeof p, "%s/c%d/userdata/.tmp", ctr, i);
    snprintf(q, sizeof q, "%s/c%d/userdata/config.json", ctr, i);
    if (writefile(p, tok, O_EXCL)) { fail("A:create-tmp", i, p); continue; }
    if (rename(p, q)) { fail("A:rename", i, q); continue; }
    checkfile("A:open-config", i, q, tok);
    snprintf(p, sizeof p, "%s/c%d/userdata", ctr, i);
    if (!listed(p, "config.json")) { errno = 0; fail("A:readdir", i, "config.json not listed"); }
    // The same without the rename: create and open at once.
    snprintf(p, sizeof p, "%s/c%d/userdata/hostname", ctr, i);
    if (writefile(p, tok, 0)) fail("A:create", i, p); else checkfile("A:open-created", i, p, tok);
    // B: rootfs/dev as a tmpfs with /dev/null bind-mounted on a file.
    snprintf(p, sizeof p, "%s/c%d/rootfs", ctr, i);
    mkdir(p, 0755);
    snprintf(p, sizeof p, "%s/c%d/rootfs/dev", ctr, i);
    if (mkdir(p, 0755)) { fail("B:mkdir-dev", i, p); continue; }
    if (mount("tmpfs", p, "tmpfs", 0, "mode=755")) { fail("B:mount-dev", i, p); continue; }
    snprintf(p, sizeof p, "%s/c%d/rootfs/dev/null", ctr, i);
    if (writefile(p, "", 0)) { fail("B:create-null", i, p); }
    else if (mount("/dev/null", p, NULL, MS_BIND, NULL)) fail("B:bind-null", i, p);
    else {
      int fd = open(p, O_RDWR);
      if (fd < 0) fail("B:open-null", i, p);
      else { if (write(fd, "x", 1) != 1) fail("B:write-null", i, p); close(fd); }
    }
    snprintf(p, sizeof p, "%s/c%d/rootfs/dev/zero", ctr, i);
    if (mknod(p, S_IFCHR | 0666, makedev(1, 5)) == 0) {
      int fd = open(p, O_RDONLY);
      if (fd < 0) fail("B:open-zero", i, p); else close(fd);
    } else if (errno != EPERM) fail("B:mknod", i, p);
    // C: only this worker's tokens are visible in the namespace's /ctr.
    DIR *d = opendir(ctr);
    struct dirent *e;
    while (d && (e = readdir(d))) {
      if (e->d_name[0] == '.') continue;
      int c;
      if (sscanf(e->d_name, "c%d", &c) != 1 || c < 0 || c > i) { errno = 0; fail("C:foreign-entry", i, e->d_name); }
    }
    if (d) closedir(d);
    // Teardown, keeping the last two directories.
    if (i >= 2) {
      snprintf(p, sizeof p, "%s/c%d/rootfs/dev/null", ctr, i - 2);
      umount2(p, MNT_DETACH);
      snprintf(p, sizeof p, "%s/c%d/rootfs/dev", ctr, i - 2);
      umount2(p, MNT_DETACH);
    }
  }
}

int main(int argc, char **argv) {
  if (argc != 4) { fprintf(stderr, "usage: fsstress BASE WORKERS ITERATIONS\n"); return 2; }
  base = argv[1];
  int workers = atoi(argv[2]), iters = atoi(argv[3]);
  char p[600];
  mkdir(base, 0755);
  snprintf(p, sizeof p, "%s/ctr", base);
  mkdir(p, 0755);
  pid_t pids[64];
  for (worker = 0; worker < workers; worker++) {
    pid_t pid = fork();
    if (pid == 0) { run(iters); _exit(fails ? 1 : 0); }
    pids[worker] = pid;
  }
  int bad = 0, st;
  for (int w = 0; w < workers; w++) {
    waitpid(pids[w], &st, 0);
    if (!WIFEXITED(st) || WEXITSTATUS(st)) bad++;
  }
  printf("fsstress %s: %d workers x %d iterations, %d workers failed\n", base, workers, iters, bad);
  return bad ? 1 : 0;
}
