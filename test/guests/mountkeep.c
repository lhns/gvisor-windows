// A tmpfs mounted on a directory of a host folder must stay mounted while the folder changes from
// outside (the harness floods it with file creations and deletions from Windows, which overflows the
// change stream). Usage: mountkeep BASE SECONDS. Exit 1 if the mount or its file vanishes.
#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

int main(int argc, char **argv) {
  if (argc != 3) { fprintf(stderr, "usage: mountkeep BASE SECONDS\n"); return 2; }
  char m[600], f[700], d[700];
  snprintf(m, sizeof m, "%s/mnt", argv[1]);
  snprintf(f, sizeof f, "%s/mnt/marker", argv[1]);
  snprintf(d, sizeof d, "%s/flood", argv[1]);
  mkdir(argv[1], 0755);
  mkdir(m, 0755);
  if (mount("tmpfs", m, "tmpfs", 0, "mode=755")) { printf("FAIL mount: %s\n", strerror(errno)); return 1; }
  int fd = open(f, O_WRONLY | O_CREAT, 0644);
  if (fd < 0) { printf("FAIL create marker: %s\n", strerror(errno)); return 1; }
  close(fd);
  printf("ready\n");
  fflush(stdout);
  time_t end = time(NULL) + atoi(argv[2]);
  long checks = 0;
  while (time(NULL) < end) {
    struct stat st;
    if (stat(f, &st)) { printf("FAIL marker gone after %ld checks: %s\n", checks, strerror(errno)); return 1; }
    // Keep dentries of the flooded directory cached, so the folder has something to invalidate.
    DIR *dir = opendir(d);
    if (dir) {
      struct dirent *e;
      int n = 0;
      while ((e = readdir(dir)) && n++ < 50) {
        char p[1000];
        snprintf(p, sizeof p, "%s/%s", d, e->d_name);
        stat(p, &st);
      }
      closedir(dir);
    }
    checks++;
    usleep(5000);
  }
  printf("mountkeep: %ld checks, mount kept\n", checks);
  return 0;
}
