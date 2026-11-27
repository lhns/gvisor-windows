// copy_file_range onto a host folder, as the vfs storage driver copies an image layer, then
// reads of the copy from another process: stat, read, mmap, and exec of a copied binary.
// Usage: cfr DIR SRC. Prints "name: ok" or the difference per step; exit 1 on a failure.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

static int fails;
static void report(const char *name, int ok, const char *detail) {
  printf("%s: %s%s%s\n", name, ok ? "ok" : "FAIL ", ok ? "" : detail, "");
  fflush(stdout);
  if (!ok) fails++;
}

static long copy(const char *src, const char *dst, int touch) {
  int in = open(src, O_RDONLY);
  int out = open(dst, O_WRONLY | O_CREAT | O_EXCL, 0755);
  if (in < 0 || out < 0) return -1;
  struct stat st;
  fstat(in, &st);
  long total = 0, n;
  while (total < st.st_size && (n = syscall(SYS_copy_file_range, in, NULL, out, NULL, st.st_size - total, 0)) > 0) total += n;
  if (touch) {
    fchown(out, 0, 0);
    fchmod(out, 0755);
    struct timespec ts[2] = {{0, 0}, {0, 0}};
    futimens(out, ts);
  }
  close(in);
  close(out);
  return total;
}

static int same(const char *a, const char *b, char *why, size_t whylen) {
  struct stat sa, sb;
  if (stat(a, &sa) || stat(b, &sb)) { snprintf(why, whylen, "stat: %s", strerror(errno)); return 0; }
  if (sa.st_size != sb.st_size) { snprintf(why, whylen, "size %ld, want %ld", (long)sb.st_size, (long)sa.st_size); return 0; }
  int fa = open(a, O_RDONLY), fb = open(b, O_RDONLY);
  char *ma = mmap(NULL, sa.st_size, PROT_READ, MAP_PRIVATE, fa, 0), *mb = mmap(NULL, sb.st_size, PROT_READ, MAP_PRIVATE, fb, 0);
  if (ma == MAP_FAILED || mb == MAP_FAILED) { snprintf(why, whylen, "mmap: %s", strerror(errno)); return 0; }
  if (memcmp(ma, mb, sa.st_size)) {
    long i = 0;
    while (ma[i] == mb[i]) i++;
    snprintf(why, whylen, "mmap differs at %ld", i);
    return 0;
  }
  char buf[832];
  if (pread(fb, buf, sizeof buf, 0) != (ssize_t)sizeof buf) { snprintf(why, whylen, "short pread"); return 0; }
  if (memcmp(buf, ma, sizeof buf)) { snprintf(why, whylen, "pread differs"); return 0; }
  close(fa); close(fb);
  munmap(ma, sa.st_size); munmap(mb, sb.st_size);
  return 1;
}

// What ld.so does: open and read the header with no stat in between.
static long rawread(const char *path) {
  int fd = open(path, O_RDONLY | O_CLOEXEC);
  char buf[832];
  long n = fd < 0 ? -1 : read(fd, buf, sizeof buf);
  close(fd);
  return n;
}


// A copy whose page cache is cut into pieces by earlier reads, then run: the Sentry maps such a
// file's text in several ranges (one per cached piece), and a `syscall` instruction at the edge of
// a range must still be patched.
static void fragmented(const char *dir, const char *src) {
  static const int strides[] = {0, 4096, 12288, 65536, 262144};
  for (unsigned k = 0; k < sizeof strides / sizeof strides[0]; k++) {
    char dst[256], name[64];
    snprintf(dst, sizeof dst, "%s/frag%u", dir, k);
    unlink(dst);
    copy(src, dst, 0);
    int fd = open(dst, O_RDONLY);
    struct stat st;
    fstat(fd, &st);
    char b;
    for (off_t o = st.st_size / 2; strides[k] && o < st.st_size; o += strides[k]) pread(fd, &b, 1, o);
    for (off_t o = 0; strides[k] && o < st.st_size / 2; o += strides[k]) pread(fd, &b, 1, o);
    close(fd);
    int pfd[2];
    pipe(pfd);
    pid_t p = fork();
    if (p == 0) {
      dup2(pfd[1], 1);
      close(pfd[0]);
      execl(dst, "sh", "-c", "echo A; echo B | cat; ls / >/dev/null; echo C", (char *)NULL);
      _exit(127);
    }
    close(pfd[1]);
    char out[64] = {0};
    int n = read(pfd[0], out, sizeof out - 1);
    int stt;
    waitpid(p, &stt, 0);
    snprintf(name, sizeof name, "run of a fragmented copy, stride %d", strides[k]);
    char why[128];
    snprintf(why, sizeof why, "status %d, output %.*s", WIFEXITED(stt) ? WEXITSTATUS(stt) : -1, n > 0 ? n : 0, out);
    report(name, WIFEXITED(stt) && WEXITSTATUS(stt) == 0 && !strcmp(out, "A\nB\nC\n"), why);
  }
}
int main(int argc, char **argv) {
  char dst[256], why[128];
  for (int touch = 0; touch < 2; touch++) {
    snprintf(dst, sizeof dst, "%s/copy%d", argv[1], touch);
    unlink(dst);
    long n = copy(argv[2], dst, touch);
    struct stat st;
    stat(argv[2], &st);
    char name[64];
    snprintf(name, sizeof name, "copy touch=%d", touch);
    snprintf(why, sizeof why, "copied %ld of %ld", n, (long)st.st_size);
    report(name, n == st.st_size, why);
    snprintf(why, sizeof why, "read of the header returned %ld", rawread(dst));
    snprintf(name, sizeof name, "raw read first, same process touch=%d", touch);
    report(name, rawread(dst) == 832, why);
    why[0] = 0;
    snprintf(name, sizeof name, "same process reads touch=%d", touch);
    report(name, same(argv[2], dst, why, sizeof why), why);
    pid_t p = fork();
    if (p == 0) {
      snprintf(why, sizeof why, "read of the header returned %ld", rawread(dst));
      snprintf(name, sizeof name, "raw read first, child touch=%d", touch);
      report(name, rawread(dst) == 832, why);
      why[0] = 0;
      snprintf(name, sizeof name, "child reads touch=%d", touch);
      report(name, same(argv[2], dst, why, sizeof why), why);
      execl(dst, "true", (char *)NULL);
      printf("exec of the copy touch=%d: FAIL %s\n", touch, strerror(errno));
      _exit(3);
    }
    int stt;
    waitpid(p, &stt, 0);
    snprintf(name, sizeof name, "exec of the copy touch=%d", touch);
    report(name, WIFEXITED(stt) && WEXITSTATUS(stt) == 0, "exit status");
  }
  fragmented(argv[1], argv[2]);
  return fails ? 1 : 0;
}
