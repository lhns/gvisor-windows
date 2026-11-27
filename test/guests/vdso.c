// vDSO guest: finds clock_gettime, gettimeofday, time and getcpu in the vDSO the
// Sentry maps (AT_SYSINFO_EHDR), calls them in loops and compares their values with
// the system calls (the Sentry's clock). It prints the number of direct system calls
// it made of each kind, so that the caller can check from the Sentry's syscall
// counters (PROFSTATS) that the vDSO calls never trapped.
#define _GNU_SOURCE
#include <elf.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/auxv.h>
#include <sys/syscall.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

typedef int (*cgt_fn)(clockid_t, struct timespec *);
typedef int (*gtod_fn)(struct timeval *, void *);
typedef time_t (*time_fn)(time_t *);
typedef long (*getcpu_fn)(unsigned *, unsigned *, void *);

static uintptr_t base, bias;
static const Elf64_Sym *syms;
static const char *strs;
static size_t nsyms;

static size_t gnu_count(const uint32_t *h) {
  uint32_t nb = h[0], symoff = h[1], bloom = h[2];
  const uint32_t *buckets = (const uint32_t *)((const uint64_t *)(h + 4) + bloom);
  const uint32_t *chain = buckets + nb;
  uint32_t last = 0;
  for (uint32_t i = 0; i < nb; i++)
    if (buckets[i] > last) last = buckets[i];
  if (last < symoff) return symoff;
  while (!(chain[last - symoff] & 1)) last++;
  return last + 1;
}

static int vdso_init(void) {
  base = getauxval(AT_SYSINFO_EHDR);
  if (!base) return -1;
  const Elf64_Ehdr *eh = (const Elf64_Ehdr *)base;
  const Elf64_Phdr *ph = (const Elf64_Phdr *)(base + eh->e_phoff);
  const Elf64_Dyn *dyn = 0;
  int found = 0;
  for (int i = 0; i < eh->e_phnum; i++) {
    if (ph[i].p_type == PT_LOAD && !found) {
      bias = base + ph[i].p_offset - ph[i].p_vaddr;
      found = 1;
    } else if (ph[i].p_type == PT_DYNAMIC) {
      dyn = (const Elf64_Dyn *)(base + ph[i].p_offset);
    }
  }
  if (!found || !dyn) return -2;
  const uint32_t *hash = 0, *ghash = 0;
  for (; dyn->d_tag != DT_NULL; dyn++) {
    uintptr_t p = dyn->d_un.d_ptr + bias;
    switch (dyn->d_tag) {
    case DT_SYMTAB: syms = (const Elf64_Sym *)p; break;
    case DT_STRTAB: strs = (const char *)p; break;
    case DT_HASH: hash = (const uint32_t *)p; break;
    case DT_GNU_HASH: ghash = (const uint32_t *)p; break;
    }
  }
  if (!syms || !strs || (!hash && !ghash)) return -3;
  nsyms = hash ? hash[1] : gnu_count(ghash);
  return 0;
}

static void *vdso_sym(const char *name) {
  char alt[64];
  snprintf(alt, sizeof alt, "__vdso_%s", name);
  for (size_t i = 0; i < nsyms; i++) {
    const Elf64_Sym *s = &syms[i];
    if (s->st_shndx == SHN_UNDEF || ELF64_ST_TYPE(s->st_info) != STT_FUNC) continue;
    const char *n = strs + s->st_name;
    if (!strcmp(n, alt) || !strcmp(n, name)) return (void *)(s->st_value + bias);
  }
  return 0;
}

static int64_t ns(const struct timespec *t) { return (int64_t)t->tv_sec * 1000000000 + t->tv_nsec; }

static long ncgt, ngtod, ntime, ngetcpu; // direct system calls made

static int64_t sys_cgt(clockid_t c) {
  struct timespec t;
  syscall(SYS_clock_gettime, c, &t);
  ncgt++;
  return ns(&t);
}

int main(void) {
  int err = vdso_init();
  if (err) {
    printf("vdso: no usable vDSO (%d)\n", err);
    return 1;
  }
  cgt_fn cgt = (cgt_fn)vdso_sym("clock_gettime");
  gtod_fn gtod = (gtod_fn)vdso_sym("gettimeofday");
  time_fn tim = (time_fn)vdso_sym("time");
  getcpu_fn gcpu = (getcpu_fn)vdso_sym("getcpu");
  if (!cgt || !gtod || !tim || !gcpu) {
    printf("vdso: missing symbols: cgt=%d gtod=%d time=%d getcpu=%d\n", !!cgt, !!gtod, !!tim, !!gcpu);
    return 1;
  }
  int fails = 0;
  // The Sentry publishes the clock parameters once its calibrated clocks are ready
  // (two samples, about a second after start); until then the vDSO falls back to the
  // system call. Wait for that outside the counted part.
  struct timespec slp = {2, 500000000};
  nanosleep(&slp, 0);

  // 1e6 clock_gettime calls through the vDSO, both clocks, timed.
  struct timespec a, b;
  int64_t t0 = sys_cgt(CLOCK_MONOTONIC);
  int64_t prev = 0;
  for (int i = 0; i < 1000000; i++) {
    cgt(i & 1 ? CLOCK_REALTIME : CLOCK_MONOTONIC, &a);
    if (!(i & 1)) {
      if (ns(&a) < prev) {
        if (fails++ < 5) printf("vdso: CLOCK_MONOTONIC went back: %lld < %lld\n", (long long)ns(&a), (long long)prev);
      }
      prev = ns(&a);
    }
  }
  int64_t t1 = sys_cgt(CLOCK_MONOTONIC);
  long per = (long)((t1 - t0) / 1000000);

  // Values against the Sentry's clock: vdso <= syscall <= vdso, within a tolerance
  // for the different reads (TSC of another CPU, rounding).
  const int64_t tol = 1000000; // 1 ms
  int64_t worst = 0;
  for (int i = 0; i < 2000; i++) {
    clockid_t c = i & 1 ? CLOCK_REALTIME : CLOCK_MONOTONIC;
    cgt(c, &a);
    int64_t s = sys_cgt(c);
    cgt(c, &b);
    int64_t lo = ns(&a) - s, hi = s - ns(&b);
    if (lo > worst) worst = lo;
    if (hi > worst) worst = hi;
    if (lo > tol || hi > tol) {
      if (fails++ < 5) printf("vdso: clock %d: vdso %lld, syscall %lld, vdso %lld\n", c, (long long)ns(&a), (long long)s, (long long)ns(&b));
    }
  }
  // gettimeofday and time, 1e5 each, plus value checks.
  struct timeval tv, stv;
  for (int i = 0; i < 100000; i++) gtod(&tv, 0);
  for (int i = 0; i < 1000; i++) {
    gtod(&tv, 0);
    syscall(SYS_gettimeofday, &stv, 0);
    ngtod++;
    int64_t d = ((int64_t)tv.tv_sec - stv.tv_sec) * 1000000 + (tv.tv_usec - stv.tv_usec);
    if (d > tol / 1000) {
      if (fails++ < 5) printf("vdso: gettimeofday %lld.%06ld after the syscall's %lld.%06ld\n", (long long)tv.tv_sec, (long)tv.tv_usec, (long long)stv.tv_sec, (long)stv.tv_usec);
    }
  }
  time_t tt = 0;
  for (int i = 0; i < 100000; i++) tt = tim(0);
  time_t st = syscall(SYS_time, 0);
  ntime++;
  if (tt > st || st - tt > 1) {
    fails++;
    printf("vdso: time %lld, syscall time %lld\n", (long long)tt, (long long)st);
  }
  // getcpu: a plausible CPU number, node 0.
  unsigned cpu = ~0u, node = ~0u;
  for (int i = 0; i < 100000; i++) {
    if (gcpu(&cpu, &node, 0) != 0 || cpu >= 1024 || node != 0) {
      if (fails++ < 5) printf("vdso: getcpu = cpu %u node %u\n", cpu, node);
      break;
    }
  }
  fprintf(stderr, "vdso: %ld ns per clock_gettime, worst deviation from the syscall %lld ns\n", per, (long long)worst);
  printf("vdso direct syscalls: clock_gettime=%ld gettimeofday=%ld time=%ld getcpu=%ld\n", ncgt, ngtod, ntime, ngetcpu);
  if (fails) {
    printf("vdso FAILED (%d)\n", fails);
    return 1;
  }
  printf("vdso ok\n");
  return 0;
}
