// apt/dpkg's file pattern, from two processes on one folder (the engine and a container on Podman's
// storage): a "container" process downloads into lists/partial/NAME, renames it over lists/NAME (which
// may exist, and may be held open by the "engine" process), reopens it, and unlinks old names; the
// "engine" process lists, opens and reads the same names, and removes finished trees (unlink every name,
// rmdir the directories), which must not fail with ENOTEMPTY or leave a name behind.
// Usage: aptrename BASE ROUNDS. Prints "FAIL tag round: error" lines; exit 1 on any.
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

static int fails;
static const char *who;

static void fail(const char *tag, int round, const char *what) {
  printf("FAIL %s %s round %d: %s: %s\n", who, tag, round, what, strerror(errno));
  fflush(stdout);
  fails++;
}

static int put(const char *path, const char *s) {
  int fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0644);
  if (fd < 0) return -1;
  if (write(fd, s, strlen(s)) < 0) { close(fd); return -1; }
  return close(fd);
}

static int content_is(const char *path, const char *want) {
  char buf[128] = {0};
  int fd = open(path, O_RDONLY);
  if (fd < 0) return -1;
  ssize_t n = read(fd, buf, sizeof buf - 1);
  close(fd);
  return n >= 0 && !strcmp(buf, want) ? 0 : -2;
}

// removetree unlinks every entry of dir and removes it.
static int removetree(const char *dir) {
  DIR *d = opendir(dir);
  if (!d) return errno == ENOENT ? 0 : -1;
  struct dirent *e;
  char p[1024];
  while ((e = readdir(d))) {
    if (!strcmp(e->d_name, ".") || !strcmp(e->d_name, "..")) continue;
    snprintf(p, sizeof p, "%s/%s", dir, e->d_name);
    struct stat st;
    if (lstat(p, &st)) { closedir(d); return -1; }
    int r = S_ISDIR(st.st_mode) ? removetree(p) : unlink(p);
    if (r) { int s = errno; closedir(d); errno = s; return -1; }
  }
  closedir(d);
  return rmdir(dir);
}

static void container(const char *base, int rounds) {
  who = "container";
  char lists[900], partial[900], p[1024], q[1024], tok[64], name[256];
  // A long name, as apt's: the path of lists/partial/NAME is well over 180 characters.
  for (int i = 0; i < rounds; i++) {
    snprintf(lists, sizeof lists, "%s/r%d/var/lib/apt/lists", base, i);
    snprintf(partial, sizeof partial, "%s/partial", lists);
    snprintf(p, sizeof p, "%s/r%d", base, i);
    mkdir(p, 0755);
    snprintf(p, sizeof p, "%s/r%d/var", base, i); mkdir(p, 0755);
    snprintf(p, sizeof p, "%s/r%d/var/lib", base, i); mkdir(p, 0755);
    snprintf(p, sizeof p, "%s/r%d/var/lib/apt", base, i); mkdir(p, 0755);
    mkdir(lists, 0755);
    if (mkdir(partial, 0755)) { fail("mkdir-partial", i, partial); continue; }
    for (int f = 0; f < 4; f++) {
      snprintf(name, sizeof name, "deb.debian.org_debian_dists_bookworm-updates_InRelease_part%d_padding_padding_padding_padding", f);
      snprintf(p, sizeof p, "%s/%s", partial, name);
      snprintf(q, sizeof q, "%s/%s", lists, name);
      snprintf(tok, sizeof tok, "r%d-f%d-old", i, f);
      if (f & 1) put(q, tok);  // an existing target
      snprintf(tok, sizeof tok, "r%d-f%d-new", i, f);
      if (put(p, tok)) { fail("create-partial", i, p); continue; }
      if (rename(p, q)) { fail("rename", i, q); continue; }
      if (content_is(q, tok) == -1) fail("open-after-rename", i, q);
      else if (content_is(q, tok) == -2) { errno = 0; fail("content-after-rename", i, q); }
      if (access(p, F_OK) == 0) { errno = 0; fail("partial-still-there", i, p); }
    }
    // Signal that this tree is complete.
    snprintf(p, sizeof p, "%s/r%d.done", base, i);
    if (put(p, "x")) fail("done-marker", i, p);
  }
}

static void engine(const char *base, int rounds) {
  who = "engine";
  char p[1024], lists[900];
  for (int i = 0; i < rounds; i++) {
    snprintf(p, sizeof p, "%s/r%d.done", base, i);
    int spins = 0;
    // While waiting, hold names open and list them, as the engine's own scans do.
    while (access(p, F_OK)) {
      snprintf(lists, sizeof lists, "%s/r%d/var/lib/apt/lists", base, i);
      DIR *d = opendir(lists);
      if (d) {
        struct dirent *e;
        while ((e = readdir(d))) {
          if (e->d_name[0] == '.' || !strcmp(e->d_name, "partial")) continue;
          char f[1024];
          snprintf(f, sizeof f, "%s/%s", lists, e->d_name);
          int fd = open(f, O_RDONLY);  // held open across the next rename
          if (fd >= 0) { char b[8]; (void)!read(fd, b, sizeof b); usleep(300); close(fd); }
        }
        closedir(d);
      }
      if (++spins > 2000000) { errno = 0; fail("timeout", i, p); return; }
      usleep(100);
    }
    // Hold one name open during the removal, closing it just after: the delete must not wait for the close.
    snprintf(lists, sizeof lists, "%s/r%d/var/lib/apt/lists", base, i);
    DIR *d = opendir(lists);
    int held = -1;
    if (d) {
      struct dirent *e;
      while ((e = readdir(d))) {
        if (e->d_name[0] == '.' || !strcmp(e->d_name, "partial")) continue;
        char f[1024];
        snprintf(f, sizeof f, "%s/%s", lists, e->d_name);
        held = open(f, O_RDONLY);
        break;
      }
      closedir(d);
    }
    snprintf(p, sizeof p, "%s/r%d", base, i);
    if (removetree(p)) fail("removetree", i, p);
    if (access(p, F_OK) == 0) { errno = 0; fail("tree-still-there", i, p); }
    if (held >= 0) close(held);
    snprintf(p, sizeof p, "%s/r%d.done", base, i);
    unlink(p);
  }
}

int main(int argc, char **argv) {
  if (argc != 3) { fprintf(stderr, "usage: aptrename BASE ROUNDS\n"); return 2; }
  const char *base = argv[1];
  int rounds = atoi(argv[2]);
  mkdir(base, 0755);
  pid_t pid = fork();
  if (pid == 0) { engine(base, rounds); _exit(fails ? 1 : 0); }
  container(base, rounds);
  int st;
  waitpid(pid, &st, 0);
  int bad = fails + (!WIFEXITED(st) || WEXITSTATUS(st));
  printf("aptrename %s: %d rounds, %s\n", base, rounds, bad ? "FAILED" : "ok");
  return bad ? 1 : 0;
}
