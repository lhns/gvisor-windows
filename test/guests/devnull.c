// The two intermittent failures of the Podman acceptance's apt build (docs/results/2r-step5-overlay.md),
// as a loop on a host-folder bind mount, as root with CAP_SYS_ADMIN and CAP_MKNOD:
//  - "cannot create /dev/null: Permission denied": the container's /dev is a tmpfs mounted on a directory of
//    the host folder, with a mknod'd character device 1:3 (mode 0666); a child drops to an unprivileged uid
//    (setgroups, setgid, setuid, as apt does for _apt) and execs a helper that opens /dev/null O_WRONLY|O_CREAT.
//  - "dpkg: cannot stat /tmp/apt-dpkg-install-*/NN-pkg.deb": the unprivileged child downloads into
//    archives/partial and renames to archives/; root symlinks it from a tmpfs directory in /tmp and stats
//    through the link, while another process renames and lists other names in the same directories.
// Usage: devnull BASE ROUNDS. Prints "FAIL ..." lines; exit 1 on any.
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#define APT_UID 100

static int fails;

static void fail(const char *what, int round, const char *path) {
  printf("FAIL round %d: %s %s: %s\n", round, what, path, strerror(errno));
  fflush(stdout);
  fails++;
}

// helper: runs as the unprivileged uid after exec; argv: devnull helper NULLPATH ARCHIVES NAME
static int helper(char **argv) {
  int fd = open(argv[2], O_WRONLY | O_CREAT | O_TRUNC, 0666);
  if (fd < 0) { printf("FAIL helper uid %d euid %d open %s: %s\n", getuid(), geteuid(), argv[2], strerror(errno)); fflush(stdout); return 10; }
  if (write(fd, "x", 1) != 1) return 11;
  close(fd);
  // apt's download method: write into partial/, rename into the archive directory.
  char from[1024], to[1024];
  snprintf(from, sizeof from, "%s/partial/%s", argv[3], argv[4]);
  snprintf(to, sizeof to, "%s/%s", argv[3], argv[4]);
  fd = open(from, O_WRONLY | O_CREAT | O_TRUNC, 0644);
  if (fd < 0) { printf("FAIL helper create %s: %s\n", from, strerror(errno)); fflush(stdout); return 12; }
  if (write(fd, "deb", 3) != 3) return 13;
  close(fd);
  if (rename(from, to)) { printf("FAIL helper rename %s: %s\n", to, strerror(errno)); fflush(stdout); return 14; }
  return 0;
}

// churn: renames and lists other names in the archive directories until the parent removes the stop file.
static void churn(const char *base) {
  char a[1024], b[1024], stop[1024];
  snprintf(stop, sizeof stop, "%s/stop", base);
  for (long i = 0;; i++) {
    struct stat st;
    if (!stat(stop, &st)) _exit(0);
    snprintf(a, sizeof a, "%s/archives/partial/churn-%ld", base, i % 7);
    snprintf(b, sizeof b, "%s/archives/churn-%ld", base, i % 5);
    int fd = open(a, O_WRONLY | O_CREAT | O_TRUNC, 0644);
    if (fd >= 0) { if (write(fd, "c", 1) < 0) {} close(fd); }
    rename(a, b);
    snprintf(a, sizeof a, "%s/archives", base);
    DIR *d = opendir(a);
    if (d) { struct dirent *e; int n = 0; while ((e = readdir(d)) && n++ < 200) { snprintf(b, sizeof b, "%s/archives/%s", base, e->d_name); lstat(b, &st); } closedir(d); }
    snprintf(b, sizeof b, "%s/archives/churn-%ld", base, (i + 2) % 5);
    unlink(b);
  }
}

int main(int argc, char **argv) {
  if (argc >= 5 && !strcmp(argv[1], "helper")) return helper(argv);
  if (argc != 3) { fprintf(stderr, "usage: devnull BASE ROUNDS\n"); return 2; }
  const char *base = argv[1];
  int rounds = atoi(argv[2]);
  char c[600], dev[700], nul[700], tmp[700], arch[700], part[700], stop[700], p[1100];
  snprintf(c, sizeof c, "%s/c", base);
  snprintf(dev, sizeof dev, "%s/c/dev", base);
  snprintf(tmp, sizeof tmp, "%s/c/tmp", base);
  snprintf(nul, sizeof nul, "%s/c/dev/null", base);
  snprintf(arch, sizeof arch, "%s/archives", base);
  snprintf(part, sizeof part, "%s/archives/partial", base);
  snprintf(stop, sizeof stop, "%s/stop", base);
  mkdir(base, 0755); mkdir(c, 0755); mkdir(dev, 0755); mkdir(tmp, 0755); mkdir(arch, 0755); mkdir(part, 0777);
  chmod(arch, 0777); chmod(part, 0777);
  unlink(stop);
  if (mount("tmpfs", dev, "tmpfs", MS_NOSUID, "mode=755,size=65536k")) { printf("FAIL mount %s: %s\n", dev, strerror(errno)); return 1; }
  if (mount("tmpfs", tmp, "tmpfs", 0, "mode=1777")) { printf("FAIL mount %s: %s\n", tmp, strerror(errno)); return 1; }
  umask(0);
  if (mknod(nul, S_IFCHR | 0666, makedev(1, 3))) { printf("FAIL mknod %s: %s\n", nul, strerror(errno)); return 1; }
  chmod(nul, 0666);
  char self[256];
  ssize_t n = readlink("/proc/self/exe", self, sizeof self - 1);
  if (n < 0) { printf("FAIL readlink exe: %s\n", strerror(errno)); return 1; }
  self[n] = 0;
  pid_t ch = fork();
  if (ch == 0) { churn(base); _exit(0); }

  for (int r = 0; r < rounds && !fails; r++) {
    char name[200], dpkg[800], link[1000], target[1000];
    snprintf(name, sizeof name, "%02d-librtmp1_2.4+20151223.gitfa8646d.1-2+b2_amd64.deb", r % 100);
    pid_t pid = fork();
    if (pid == 0) {
      if (setgroups(0, NULL) || setgid(APT_UID) || setuid(APT_UID)) _exit(20);
      char *a[] = {self, "helper", nul, arch, name, NULL};
      execv(self, a);
      _exit(21);
    }
    int st = 0;
    waitpid(pid, &st, 0);
    if (!WIFEXITED(st) || WEXITSTATUS(st)) { snprintf(p, sizeof p, "status 0x%x", st); fail("helper", r, p); break; }
    // dpkg side, as root: a directory in /tmp (tmpfs) with a symlink to the archive, stat through it.
    snprintf(dpkg, sizeof dpkg, "%s/apt-dpkg-install-%d", tmp, r);
    snprintf(link, sizeof link, "%s/%s", dpkg, name);
    snprintf(target, sizeof target, "%s/%s", arch, name);
    if (mkdir(dpkg, 0700)) { fail("mkdir", r, dpkg); break; }
    if (symlink(target, link)) { fail("symlink", r, link); break; }
    struct stat sb;
    if (stat(link, &sb)) { fail("stat through symlink", r, link); break; }
    if (lstat(target, &sb)) { fail("lstat target", r, target); break; }
    unlink(link);
    rmdir(dpkg);
    unlink(target);
  }
  int fd = open(stop, O_WRONLY | O_CREAT, 0644);
  if (fd >= 0) close(fd);
  waitpid(ch, NULL, 0);
  printf("devnull: %d rounds, %d failures\n", rounds, fails);
  return fails ? 1 : 0;
}
