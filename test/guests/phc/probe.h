// Heap probes for the rewrite-mode heap corruption (docs/results/1p-rewrite-heap.md): every DIR the
// program opens is recorded with its chunk header; readdir and closedir check that neither the
// pointer nor the header changed, and SIGABRT dumps what is known.
#include <asm/prctl.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

static DIR *probe_dirs[64];
static size_t probe_hdr[64];
static size_t probe_prev[64];
static unsigned long probe_opens, probe_bad;

static void probe_out(const char *s) { (void)!write(2, s, strlen(s)); }

static void probe_hex(const char *what, const void *p, int n) {
  char b[512];
  int k = snprintf(b, sizeof b, "PROBE %s %p:", what, p);
  for (int i = 0; i < n && k < (int)sizeof b - 20; i += 8)
    k += snprintf(b + k, sizeof b - k, " %016lx", *(const unsigned long *)((const char *)p + i));
  snprintf(b + k, sizeof b - k, "\n");
  probe_out(b);
}

static void probe_state(const char *why) {
  char b[512];
  unsigned long fs = 0;
  syscall(SYS_arch_prctl, ARCH_GET_FS, &fs);
  snprintf(b, sizeof b, "PROBE %s: pid %d fs %#lx brk %p &errno %p stdout buf %p-%p opens %lu\n", why, getpid(), fs,
           sbrk(0), (void *)&errno, (void *)stdout->_IO_buf_base, (void *)stdout->_IO_buf_end, probe_opens);
  probe_out(b);
  for (int i = 0; i < 64; i++) {
    if (!probe_dirs[i]) continue;
    snprintf(b, sizeof b, "PROBE dir %d %p hdr %#zx (was %#zx) prev %#zx (was %#zx)\n", i, (void *)probe_dirs[i],
             ((size_t *)probe_dirs[i])[-1], probe_hdr[i], ((size_t *)probe_dirs[i])[-2], probe_prev[i]);
    probe_out(b);
    probe_hex("around", (char *)probe_dirs[i] - 64, 128);
  }
  if (stdout->_IO_buf_base) probe_hex("stdout chunk", stdout->_IO_buf_base - 16, 48);
  int fd = open("/proc/self/maps", O_RDONLY);
  if (fd >= 0) {
    char m[4096];
    ssize_t n;
    while ((n = read(fd, m, sizeof m)) > 0) (void)!write(2, m, n);
    close(fd);
  }
}

static void probe_abrt(int sig) {
  (void)sig;
  probe_state("SIGABRT");
  signal(SIGABRT, SIG_DFL);
  raise(SIGABRT);
}

static int probe_find(DIR *d) {
  for (int i = 0; i < 64; i++)
    if (probe_dirs[i] == d) return i;
  return -1;
}

static void probe_check(DIR *d, const char *where) {
  int i = probe_find(d);
  if (i < 0) {
    char b[160];
    snprintf(b, sizeof b, "PROBE %s: DIR %p was never opened (pointer changed)\n", where, (void *)d);
    probe_out(b);
    probe_bad++;
    probe_state(where);
    return;
  }
  if (((size_t *)d)[-1] != probe_hdr[i]) {
    char b[160];
    snprintf(b, sizeof b, "PROBE %s: header of DIR %p changed\n", where, (void *)d);
    probe_out(b);
    probe_bad++;
    probe_state(where);
    probe_hdr[i] = ((size_t *)d)[-1];
  }
}

static DIR *probe_opendir(const char *p) {
  static int init;
  if (!init) { init = 1; signal(SIGABRT, probe_abrt); }
  DIR *d = opendir(p);
  if (d) {
    probe_opens++;
    int i = probe_find(0);
    if (i >= 0) { probe_dirs[i] = d; probe_hdr[i] = ((size_t *)d)[-1]; probe_prev[i] = ((size_t *)d)[-2]; }
    if (probe_opens == 1) probe_state("first opendir");
  }
  return d;
}

static struct dirent *probe_readdir(DIR *d) {
  probe_check(d, "before readdir");
  struct dirent *e = readdir(d);
  probe_check(d, "after readdir");
  return e;
}

static int probe_closedir(DIR *d) {
  probe_check(d, "before closedir");
  int i = probe_find(d);
  if (i >= 0) probe_dirs[i] = 0;
  return closedir(d);
}
