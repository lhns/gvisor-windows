// Shrinks the rewrite-mode heap corruption (docs/results/1p-rewrite-heap.md).
// Usage: heapprobe MODES DIR N. MODES letters: f the work runs in a forked child (c: the parent
// meanwhile renames files in DIR, as aptrename's container does); e each iteration fails an
// access() (errno write); m mallocs and frees; d opendir/readdir/closedir of DIR; p printf first.
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include "probe.h"

static int has(const char *m, char c) { return strchr(m, c) != NULL; }

static int work(const char *m, const char *dir, int n) {
  char miss[1024];
  snprintf(miss, sizeof miss, "%s/does-not-exist", dir);
  if (has(m, 'p')) printf("heapprobe: work in pid %d\n", getpid());
  void *keep[16] = {0};
  for (int i = 0; i < n; i++) {
    if (has(m, 'e')) for (int k = 0; k < 10; k++) (void)access(miss, F_OK);
    if (has(m, 'm')) {
      int k = i % 16;
      free(keep[k]);
      keep[k] = malloc(16 + (size_t)(i * 7919 % 40000));
      if (keep[k]) memset(keep[k], 0x5a, 16);
    }
    if (has(m, 'd')) {
      DIR *d = probe_opendir(dir);
      if (!d) { perror("opendir"); return 1; }
      struct dirent *e;
      while ((e = probe_readdir(d))) { if (has(m, 'e')) (void)access(miss, F_OK); }
      probe_closedir(d);
    }
  }
  for (int k = 0; k < 16; k++) free(keep[k]);
  printf("heapprobe %s: %d iterations in pid %d, %lu bad\n", m, n, getpid(), probe_bad);
  return probe_bad ? 1 : 0;
}

int main(int argc, char **argv) {
  if (argc != 4) { fprintf(stderr, "usage: heapprobe MODES DIR N\n"); return 2; }
  const char *m = argv[1], *dir = argv[2];
  int n = atoi(argv[3]);
  mkdir(dir, 0755);
  for (int k = 0; k < 8; k++) {
    char p[1024];
    snprintf(p, sizeof p, "%s/file%d", dir, k);
    int fd = open(p, O_WRONLY | O_CREAT | O_TRUNC, 0644);
    if (fd >= 0) close(fd);
  }
  if (!has(m, 'f')) return work(m, dir, n);
  pid_t pid = fork();
  if (pid == 0) _exit(work(m, dir, n));
  if (has(m, 'c')) {
    char p[1024], q[1024];
    snprintf(p, sizeof p, "%s/.tmp", dir);
    snprintf(q, sizeof q, "%s/file0", dir);
    int st;
    while (waitpid(pid, &st, WNOHANG) == 0) {
      int fd = open(p, O_WRONLY | O_CREAT | O_TRUNC, 0644);
      if (fd >= 0) { (void)!write(fd, "x", 1); close(fd); }
      rename(p, q);
      (void)access(p, F_OK);
    }
    printf("heapprobe parent: child status %#x\n", st);
    return !WIFEXITED(st) || WEXITSTATUS(st);
  }
  int st;
  waitpid(pid, &st, 0);
  printf("heapprobe parent: child status %#x\n", st);
  return !WIFEXITED(st) || WEXITSTATUS(st);
}
