// ptrace tracer: follows fork/clone/exec and counts executed `syscall` sites as (mapped file, file offset, nr).
// Usage: tracer OUT.tsv -- cmd args...   (static build: gcc -O2 -static tracer.c -o tracer)
// Output lines: path <TAB> file offset of the syscall instruction <TAB> syscall nr <TAB> count
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/ptrace.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ptrace.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#ifndef PTRACE_GET_SYSCALL_INFO
#define PTRACE_GET_SYSCALL_INFO 0x420e
#endif

struct range { uint64_t start, end, pgoff; int path; };
struct tmaps { struct range *r; int n; };

static char **paths; static int npaths, cappaths;
static int intern(const char *p) {
	for (int i = 0; i < npaths; i++) if (!strcmp(paths[i], p)) return i;
	if (npaths == cappaths) { cappaths = cappaths ? cappaths * 2 : 64; paths = realloc(paths, cappaths * sizeof *paths); }
	paths[npaths] = strdup(p);
	return npaths++;
}

static struct tmaps **maps; static long maxtid;
static void load_maps(pid_t tid) {
	if (tid >= maxtid) return;
	if (maps[tid]) { free(maps[tid]->r); free(maps[tid]); maps[tid] = NULL; }
	char fn[64]; snprintf(fn, sizeof fn, "/proc/%d/maps", tid);
	FILE *f = fopen(fn, "r"); if (!f) return;
	struct tmaps *m = calloc(1, sizeof *m); int cap = 0; char line[4096];
	while (fgets(line, sizeof line, f)) {
		uint64_t s, e, off; char perms[8]; char path[3000] = "";
		int n = sscanf(line, "%lx-%lx %7s %lx %*s %*s %2999[^\n]", &s, &e, perms, &off, path);
		if (n < 4 || perms[2] != 'x') continue;
		char *p = path; while (*p == ' ') p++;
		if (m->n == cap) { cap = cap ? cap * 2 : 32; m->r = realloc(m->r, cap * sizeof *m->r); }
		m->r[m->n++] = (struct range){s, e, off, intern(*p ? p : "[anon]")};
	}
	fclose(f); maps[tid] = m;
}
static struct range *lookup(pid_t tid, uint64_t a) {
	for (int pass = 0; pass < 2; pass++) {
		if (tid < maxtid && maps[tid]) for (int i = 0; i < maps[tid]->n; i++)
			if (a >= maps[tid]->r[i].start && a < maps[tid]->r[i].end) return &maps[tid]->r[i];
		if (pass == 0) load_maps(tid);
	}
	return NULL;
}

struct ent { int path; uint64_t off; int nr; uint64_t cnt; };
#define TSZ (1 << 20)
static struct ent tab[TSZ]; static int used[TSZ];
static void bump(int path, uint64_t off, int nr) {
	uint64_t h = (off * 0x9E3779B97F4A7C15ull) ^ ((uint64_t)path << 20) ^ (uint64_t)nr * 0x85EBCA6Bull;
	for (uint32_t i = h & (TSZ - 1);; i = (i + 1) & (TSZ - 1)) {
		if (!used[i]) { used[i] = 1; tab[i] = (struct ent){path, off, nr, 1}; return; }
		if (tab[i].path == path && tab[i].off == off && tab[i].nr == nr) { tab[i].cnt++; return; }
	}
}

int main(int argc, char **argv) {
	if (argc < 4 || strcmp(argv[2], "--")) { fprintf(stderr, "usage: tracer OUT -- cmd...\n"); return 2; }
	FILE *pm = fopen("/proc/sys/kernel/pid_max", "r"); long pmax = 4194304; if (pm) { if (fscanf(pm, "%ld", &pmax) != 1) pmax = 4194304; fclose(pm); }
	maxtid = pmax + 1; maps = calloc(maxtid, sizeof *maps);
	pid_t child = fork();
	if (!child) { ptrace(PTRACE_TRACEME, 0, 0, 0); raise(SIGSTOP); execvp(argv[3], argv + 3); perror("exec"); _exit(127); }
	int st; waitpid(child, &st, 0);
	ptrace(PTRACE_SETOPTIONS, child, 0, PTRACE_O_TRACESYSGOOD | PTRACE_O_TRACEFORK | PTRACE_O_TRACEVFORK |
		PTRACE_O_TRACECLONE | PTRACE_O_TRACEEXEC | PTRACE_O_EXITKILL);
	ptrace(PTRACE_SYSCALL, child, 0, 0);
	long total = 0, unmapped = 0; int rc = 0;
	for (;;) {
		pid_t pid = waitpid(-1, &st, __WALL);
		if (pid < 0) { if (errno == EINTR) continue; break; }
		if (WIFEXITED(st) || WIFSIGNALED(st)) { if (pid == child) rc = WIFEXITED(st) ? WEXITSTATUS(st) : 128 + WTERMSIG(st);
			if (pid < maxtid && maps[pid]) { free(maps[pid]->r); free(maps[pid]); maps[pid] = NULL; } continue; }
		if (!WIFSTOPPED(st)) continue;
		int sig = WSTOPSIG(st), ev = st >> 16, inject = 0;
		if (sig == (SIGTRAP | 0x80)) {
			struct ptrace_syscall_info si; memset(&si, 0, sizeof si);
			if (ptrace(PTRACE_GET_SYSCALL_INFO, pid, (void *)sizeof si, &si) > 0 && si.op == PTRACE_SYSCALL_INFO_ENTRY) {
				total++;
				uint64_t site = si.instruction_pointer - 2;
				struct range *r = lookup(pid, site);
				if (r) bump(r->path, site - r->start + r->pgoff, (int)si.entry.nr); else unmapped++;
			}
		} else if (sig == SIGTRAP && ev) {
			if (ev == PTRACE_EVENT_EXEC && pid < maxtid && maps[pid]) { free(maps[pid]->r); free(maps[pid]); maps[pid] = NULL; }
		} else if (sig == SIGSTOP) {
		} else inject = sig;
		ptrace(PTRACE_SYSCALL, pid, 0, inject);
	}
	FILE *o = fopen(argv[1], "w");
	for (int i = 0; i < TSZ; i++) if (used[i]) fprintf(o, "%s\t%lu\t%d\t%lu\n", paths[tab[i].path], tab[i].off, tab[i].nr, tab[i].cnt);
	fprintf(o, "#total\t%ld\t#unmapped\t%ld\n", total, unmapped);
	fclose(o);
	return rc;
}
