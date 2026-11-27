// driver runs gVisor's syscall test cases one by one inside the guest and
// prints one result line per case, so a single kernel (the Sentry on Windows,
// or runsc on Linux) runs many test binaries and a crash loses only what
// follows. Usage: driver [-t seconds] list.txt
//
// Each line of list.txt is "binary<TAB>test", the test being a gtest name as
// printed by --gtest_list_tests (Suite.Case or Suite/Param.Case). The case
// runs as /bin/binary --gtest_filter=test with its own TEST_TMPDIR.
//
// Output lines (tab separated):
//   S binary test                            before a case starts (so a crash names its case)
//   R binary test status code milliseconds   status: PASS FAIL SKIP TIMEOUT SIGNAL NOTRUN
//   O escaped-tail-of-output                 only for statuses other than PASS
//   DONE count

#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static long now_ms(void) {
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return ts.tv_sec * 1000L + ts.tv_nsec / 1000000L;
}

static void print_escaped(const char* s, size_t n) {
  for (size_t i = 0; i < n; i++) {
    unsigned char c = s[i];
    if (c == '\n') fputs("\\n", stdout);
    else if (c == '\t') fputc(' ', stdout);
    else if (c < 32 || c > 126) fputc('?', stdout);
    else fputc(c, stdout);
  }
}

static void rm_rf(const char* dir) {
  pid_t p = fork();
  if (p == 0) {
    int null = open("/dev/null", O_RDWR);
    if (null >= 0) {
      dup2(null, 1);
      dup2(null, 2);
    }
    execl("/bin/busybox", "busybox", "rm", "-rf", dir, (char*)NULL);
    _exit(127);
  }
  if (p > 0) waitpid(p, NULL, 0);
}

int main(int argc, char** argv) {
  int timeout = 120;
  int i = 1;
  if (i + 1 < argc && strcmp(argv[i], "-t") == 0) {
    timeout = atoi(argv[i + 1]);
    i += 2;
  }
  if (i >= argc) {
    fprintf(stderr, "usage: driver [-t seconds] list.txt\n");
    return 2;
  }
  FILE* list = fopen(argv[i], "r");
  if (!list) {
    perror("list");
    return 2;
  }
  setvbuf(stdout, NULL, _IOLBF, 0);
  mkdir("/tmp", 0777);
  char line[4096];
  long count = 0;
  while (fgets(line, sizeof line, list)) {
    size_t len = strlen(line);
    while (len > 0 && (line[len - 1] == '\n' || line[len - 1] == '\r')) line[--len] = 0;
    char* tab = strchr(line, '\t');
    if (!tab) continue;
    *tab = 0;
    const char* bin = line;
    const char* test = tab + 1;
    char path[512], filter[2048], tmpdir[128], tmpenv[160];
    snprintf(path, sizeof path, "/bin/%s", bin);
    snprintf(filter, sizeof filter, "--gtest_filter=%s", test);
    snprintf(tmpdir, sizeof tmpdir, "/tmp/t%ld", count);
    snprintf(tmpenv, sizeof tmpenv, "TEST_TMPDIR=%s", tmpdir);
    mkdir(tmpdir, 0777);
    count++;

    int pfd[2];
    if (pipe(pfd) < 0) {
      perror("pipe");
      return 2;
    }
    printf("S\t%s\t%s\n", bin, test);
    long t0 = now_ms();
    pid_t pid = fork();
    if (pid < 0) {
      printf("R\t%s\t%s\tFAIL\tfork:%d\t0\n", bin, test, errno);
      close(pfd[0]);
      close(pfd[1]);
      continue;
    }
    if (pid == 0) {
      setpgid(0, 0);
      close(pfd[0]);
      int null = open("/dev/null", O_RDONLY);
      if (null >= 0) dup2(null, 0);
      dup2(pfd[1], 1);
      dup2(pfd[1], 2);
      close(pfd[1]);
      char* envp[] = {tmpenv, "TEST_ON_GVISOR=systrap", "TEST_SRCDIR=/runfiles", "HOME=/",
                      "PATH=/bin:/usr/bin:/", "TEST_UNDECLARED_OUTPUTS_DIR=/tmp", NULL};
      char* args[] = {path, filter, NULL};
      execve(path, args, envp);
      dprintf(2, "driver: exec %s: %s\n", path, strerror(errno));
      _exit(127);
    }
    close(pfd[1]);
    char tail[4096];
    size_t tl = 0;
    int status = 0, timedout = 0, reaped = 0;
    char buf[2048];
    int eof = 0;
    while (!reaped) {
      struct pollfd p = {pfd[0], POLLIN, 0};
      int r = eof ? 0 : poll(&p, 1, 50);
      if (r > 0) {
        ssize_t n = read(pfd[0], buf, sizeof buf);
        if (n > 0) {
          if (n >= (ssize_t)sizeof tail) {
            memcpy(tail, buf + n - sizeof tail, sizeof tail);
            tl = sizeof tail;
          } else {
            if (tl + n > sizeof tail) {
              size_t drop = tl + n - sizeof tail;
              memmove(tail, tail + drop, tl - drop);
              tl -= drop;
            }
            memcpy(tail + tl, buf, n);
            tl += n;
          }
        } else {
          eof = 1;
        }
      } else if (eof) {
        usleep(20000);
      }
      pid_t w = waitpid(pid, &status, WNOHANG);
      if (w == pid) {
        reaped = 1;
      } else if (now_ms() - t0 > timeout * 1000L && !timedout) {
        timedout = 1;
        kill(-pid, SIGKILL);
        kill(pid, SIGKILL);
      }
    }
    // Drain what is left (a child that ran past its parent may keep the pipe open).
    fcntl(pfd[0], F_SETFL, O_NONBLOCK);
    for (;;) {
      ssize_t n = read(pfd[0], buf, sizeof buf);
      if (n <= 0) break;
      if ((size_t)n >= sizeof tail) n = sizeof tail;
      if (tl + n > sizeof tail) {
        size_t drop = tl + n - sizeof tail;
        memmove(tail, tail + drop, tl - drop);
        tl -= drop;
      }
      memcpy(tail + tl, buf, n);
      tl += n;
    }
    close(pfd[0]);
    kill(-pid, SIGKILL);
    long ms = now_ms() - t0;

    const char* st;
    int code = 0;
    tail[tl < sizeof tail ? tl : sizeof tail - 1] = 0;
    if (timedout) {
      st = "TIMEOUT";
    } else if (WIFSIGNALED(status)) {
      st = "SIGNAL";
      code = WTERMSIG(status);
    } else {
      code = WEXITSTATUS(status);
      if (code == 0) {
        if (strstr(tail, "[  SKIPPED ]")) st = "SKIP";
        else if (strstr(tail, "Running 0 tests") || strstr(tail, "[  PASSED  ] 0 tests")) st = "NOTRUN";
        else st = "PASS";
      } else {
        st = "FAIL";
      }
    }
    printf("R\t%s\t%s\t%s\t%d\t%ld\n", bin, test, st, code, ms);
    if (strcmp(st, "PASS") != 0) {
      fputs("O\t", stdout);
      print_escaped(tail, tl);
      fputc('\n', stdout);
    }
    rm_rf(tmpdir);
  }
  printf("DONE\t%ld\n", count);
  return 0;
}
