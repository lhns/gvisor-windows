# 0d: Syscall sites and faults per fork+exec (Linux, input to ADR 0010)

CI run: https://github.com/lhns/gvisor-windows/actions/runs/45862412661 (workflow `.github/workflows/syscall-survey.yml`, branch `syscall-site-survey`, ubuntu-latest x86_64; artifacts `sites-*` and `faults`).
Sources: `test/e2e/syscall-survey/` (`survey.py`, `tracer.c`, `join.py`, `run-image.sh`, `faults.sh`, `faults-report.py`).

## A. Syscall sites

### Method
- Images (mirror.gcr.io/library unless noted): alpine:3.20, debian:12, python:3-slim, nginx:latest, `quay.io/podman/stable:latest` (digest `sha256:a02eea67...`, created 2026-11-26; the podman version was not recorded, so "6.x" is not verified), plus a static Go hello world (go1.27.4, `CGO_ENABLED=0`, imports `os/exec`).
- `docker export` each image, take every x86-64 ELF (executables and shared objects; hardlinks and byte-identical copies counted once), run `objdump -d` (linear sweep) and find every `syscall` (0f 05), `sysenter`, `int $0x80`. The last two are almost absent (last column of the first table) and are not classified.
- Classes, decided by looking up to 10 instructions back inside the same function:
  1. directly preceded by `mov $imm32,%eax` (`b8 imm32`): the 7-byte sequence `usertrap_amd64.go` patches into `jmp *0x60000+n` (it checks byte 0xb8 at IP-7 and nothing else)
  2. eax/rax set from an immediate or zeroed earlier, or with another encoding (`xor %eax,%eax`, `mov $imm,%rax` = 48 c7 c0, a mov that is not adjacent): patchable by a slightly extended patcher
  3. eax/rax loaded from a register or memory (glibc/musl `syscall()` and cancellable wrappers, computed numbers)
  4. no write to eax/rax found in the window, or only a `call` before it. In practice this is the Go regabi `internal/runtime/syscall.Syscall6`, which receives the number in AX as its first argument; it is a register-sourced number like class 3.
- "Relocatable" (a `jmp rel32` could replace the site): a contiguous window of at least 5 bytes made of up to 3 instructions before and 3 after the `syscall`, none rip-relative or a branch/call/ret, and no direct branch target on an interior instruction start. Jump tables and other indirect targets are invisible to this check, so it is optimistic. Result (`reloc5` in `static.json`): it holds for every site of every class except 15 class 2 sites in the podman image and 14 class 3 sites in nginx (libgomp). Treat it as "almost always possible", not as proof.
- Dynamic: a ptrace tracer (`tracer.c`, follows fork/clone/exec) runs the workload inside the same image (docker, `--privileged`) and records, for every syscall entry, the mapped file and file offset of the `syscall` instruction (rip-2 mapped through `/proc/<tid>/maps`). It is joined with the static table by (file content hash, file offset). `strace -k` was rejected: its frames are symbol+offset or absolute addresses, which do not map reliably to instruction sites. Workloads run natively on the runner, not under gVisor. "not in table" = vdso fallbacks and the tracer's own few calls.

### Static counts per image (unique ELFs; the podman image also contains qemu-user-static binaries with about 340 sites each, which inflate its total)

| image | ELFs (unique) | sites | class 1 | class 2 | class 3 | class 4 | sysenter/int 0x80 |
|---|---|---|---|---|---|---|---|
| alpine | 17 | 505 | 232 (46%) | 125 (25%) | 143 (28%) | 5 (1%) | 2 |
| debian | 694 | 722 | 590 (82%) | 83 (11%) | 49 (7%) | 0 (0%) | 1 |
| python | 747 | 598 | 470 (79%) | 83 (14%) | 45 (8%) | 0 (0%) | 0 |
| nginx | 832 | 636 | 470 (74%) | 84 (13%) | 82 (13%) | 0 (0%) | 0 |
| podman | 991 | 12056 | 9950 (83%) | 1885 (16%) | 218 (2%) | 3 (0%) | 7 |
| gohello | 1 | 39 | 35 (90%) | 1 (3%) | 2 (5%) | 1 (3%) | 0 |

### Static counts per binary of interest

| binary | size | sites | class 1 | class 2 | class 3 | class 4 |
|---|---|---|---|---|---|---|
| podman: /usr/bin/podman | 47055 KiB | 41 | 36 | 1 | 3 | 1 |
| podman: /usr/bin/conmon | 166 KiB | 0 | 0 | 0 | 0 | 0 |
| podman: /usr/bin/crun | 671 KiB | 0 | 0 | 0 | 0 | 0 |
| podman: /usr/libexec/podman/netavark | 8892 KiB | 38 | 4 | 34 | 0 | 0 |
| podman: /usr/libexec/podman/aardvark-dns | 2027 KiB | 0 | 0 | 0 | 0 | 0 |
| podman: /usr/lib64/ld-linux-x86-64.so.2 | 1033 KiB | 63 | 46 | 17 | 0 | 0 |
| podman: /usr/lib64/libc.so.6 | 2436 KiB | 484 | 408 | 69 | 7 | 0 |
| gohello: /hello | 2839 KiB | 39 | 35 | 1 | 2 | 1 |
| debian: /usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 | 209 KiB | 46 | 33 | 9 | 4 | 0 |
| debian: /usr/lib/x86_64-linux-gnu/libc.so.6 | 1881 KiB | 526 | 436 | 54 | 36 | 0 |
| alpine: /bin/busybox | 789 KiB | 0 | 0 | 0 | 0 | 0 |
| alpine: /lib/ld-musl-x86_64.so.1 | 634 KiB | 504 | 232 | 125 | 143 | 4 |

conmon, crun and aardvark-dns contain no `syscall` instruction at all: they go through libc, so their syscalls come from the libc sites above. podman (Go) has 41 sites in 47 MB, all in the Go runtime's assembly; netavark (Rust) has 38, mostly class 2.

### Dynamic share (syscalls executed, by class of the issuing site)

| workload | syscalls | class 1 | class 2 | class 3 | class 4 | not in table | distinct sites hit |
|---|---|---|---|---|---|---|---|
| alpine: shell-script | 5865 | 23.0% | 49.3% | 27.6% | 0.0% | 0.1% | 41 |
| debian: apt-get-update | 23182 | 71.6% | 28.0% | 0.4% | 0.0% | 0.0% | 99 |
| debian: shell-script | 8941 | 81.4% | 16.8% | 1.7% | 0.0% | 0.0% | 55 |
| python: import | 2681 | 67.6% | 3.7% | 28.5% | 0.0% | 0.2% | 59 |
| python: shell-script | 16792 | 66.0% | 24.7% | 9.2% | 0.0% | 0.0% | 55 |
| nginx: nginx-start | 715 | 61.5% | 10.9% | 27.0% | 0.0% | 0.6% | 71 |
| nginx: nginx-t | 289 | 73.4% | 13.8% | 11.4% | 0.0% | 1.4% | 52 |
| podman: podman-info | 8311 | 72.1% | 8.5% | 1.8% | 17.4% | 0.2% | 120 |
| podman: podman-version | 2426 | 66.6% | 5.4% | 5.0% | 22.8% | 0.2% | 101 |
| gohello: hello-exec | 501 | 77.6% | 3.0% | 1.0% | 17.6% | 0.8% | 54 |
| gohello: hello | 308 | 82.1% | 2.9% | 0.3% | 13.3% | 1.3% | 52 |

Workloads: `shell-script` = 25 iterations of `/bin/true; ls /etc; cat /etc/passwd | grep root; echo | sed` (plus `apk --version` on alpine); `apt-get update` with network; python imports json, re, os, asyncio, sqlite3, decimal, ssl; `nginx -g 'daemon off;'` for 3 s and `nginx -t`; `podman info`, and `podman version; crun --version; conmon --version; netavark --version`; Go hello with and without spawning `/bin/true` (the child's libc sites come from the debian table).

### Findings
- Stock usertrap (class 1 only) covers 62-82% of executed syscalls on glibc and Go, but only 23% on musl (alpine): musl wrappers take the number from a register or set it with another encoding (classes 2 and 3).
- The largest class 2 site is libc's `read` (`xor %eax,%eax; syscall`): 4849 of 23182 syscalls in `apt-get update` (21%), 2825 of 16792 in the python shell script (17%). Matching `xor %eax,%eax; syscall` and `mov $imm,%rax; syscall` is cheap and lifts glibc shell/apt workloads to 91-99%.
- The remainder are register-sourced numbers: Go's `Syscall6` (one site, 13-23% of the syscalls of a Go program: fcntl, close, read, epoll_ctl, newfstatat, openat from the standard library), glibc's cancellable wrappers (class 3 is 28% of the python import) and musl. A patcher that replaces 5 bytes at the site with `jmp rel32` to a trampoline that reads the live rax covers these when the window is relocatable.
- Cost model for the debugger path: each distinct site traps once per address space; an exec starts from unpatched pages, so it re-traps. Workloads above hit 41-120 distinct sites, i.e. 5-14 ms per process at about 120 us per debug event. Sites that stock usertrap cannot patch (classes 2-4) trap on every execution: for `podman info` that is 28% of 8311 syscalls, about 2300 events (0.28 s) plus about 95 first traps, against 1.0 s with no patching at all.

**Verdict A:** stock usertrap patching covers only 62-82% of executed syscalls (23% on musl); matching `xor %eax,%eax`/`mov $imm,%rax` and adding a register-number trampoline with a 5-byte relocating jump reaches essentially all sites, leaving the first trap of each distinct site per process (40-120 sites, 5-14 ms) as the real price of the debugger design.

## B. Faults per fork+exec under runsc (systrap)

### Counter
The Sentry's own metric `/task/faults` (`pkg/sentry/kernel/task.go`, incremented in `task_run.go` for every application fault that goes to `MemoryManager.HandleUserFault`, i.e. exactly the events a Windows debugger would deliver). `/proc/self/stat` reports minflt as 0 (`pkg/sentry/fsimpl/proc/task_files.go`) and `getrusage` does not support ru_minflt, so those cannot be used. The metric is only compiled with the `sentry_profiling` build tag, which the release binaries lack; `faults.sh` therefore builds `runsc` and `gvisor_sentry` from this tree with `-tags sentry_profiling`, `CGO_ENABLED=0`, and runs them with `--profiling-metrics=/task/faults,/task/syscalls --profiling-metrics-log=... --platform=systrap --network=host` (500 us snapshots, last value taken). The release bundle's other sidecars are used unchanged. runsc runs `sh -c 'i=0; while [ $i -lt N ]; do <workload>; i=$((i+1)); done'` on a docker-exported rootfs; the per-exec cost is the slope between N=0 (sandbox start plus shell, subtracted) and N>0.

### Result

| workload (fork+exec+wait from dash, in a loop) | faults per exec | syscalls per exec | faults at N=0 |
|---|---|---|---|
| `/bin/true` (debian, dynamic glibc) | 19.1 | 38.0 | 27 |
| static Go hello world | 10.5 | 309-312 | 28 |
| `python3 -c pass` (python:3-slim) | 56.7 | 326-334 | 29 |

Two values of N per workload (40/120 and 15/45) give the same slope to within 0.1 fault. At about 120 us per debug event the fault part is 2.3 ms, 1.3 ms and 6.8 ms per exec. For the Go and python cases this is an order of magnitude below the syscall events if syscalls were not patched (about 310-330 per exec).

**Verdict B:** a fork+exec costs 10-57 Sentry-handled faults (19 for `/bin/true`, 10.5 for static Go, 57 for python), about 1-7 ms of debug-event time, not tens of milliseconds.

### How much is mapped per fault (pkg/sentry/mm)
There is no `privateAllocUnit` in this tree; the logic is in `HandleUserFault` (`syscalls.go`), `getPMAsInternalLocked` (`pma.go`) and `mapASLocked` (`address_space.go`):
1. `HandleUserFault` asks for the single faulting page. `getPMAsLocked` finds or creates pmas (physical memory areas) for it, then `mapASLocked` maps them into the AddressSpace; `platform.MapUnit()` decides how far to over-map.
2. Creating pmas over-allocates "to reduce application page faults":
   - private anonymous memory: the faulting page rounded out to the 2 MiB huge-page boundary (`hugepageAligned`), clipped to the vma and the gap.
   - file-backed: `Mappable.Translate(required, optional)` gets the page as `required` and the rest of the vma gap as `optional`. tmpfs fills around the fault in 64 KiB units (2 MiB for huge tmpfs); a gofer file with a host fd returns the whole `optional` range as one translation of the host file; other gofer files fill the page cache up to `maxFillRange`.
   - copy-on-write break after fork: executable vmas copy only the required pages ("not enough spatial locality"), stacks the page plus one before and after, other vmas the 2 MiB-aligned range.
3. `mapASLocked`: with `MapUnit() == 0` ("cost independent of pages mapped"; systrap and ptrace return 0) `mapAR` is unbounded, so one fault maps every overlapped pma completely, up to the 2 MiB region or the whole file translation. With a non-zero unit (KVM, SlimVM) only `ar` rounded out to the unit is mapped. `precommit` (`PlatformEffectCommit`) is true only for explicit commit requests (mlock, MAP_POPULATE); then only `ar` itself is mapped. It is never set on the fault path (`PlatformEffectDefault`).
4. So the floor is roughly one fault per (region x permission transition) the guest touches: first touch of each segment of each loaded ELF and libc, a second fault when a read-mapped COW private page is written, stack growth, and the parent shell's COW faults after fork. That fits 10 faults for static Go (one image, no ld.so) and 19 for `/bin/true` (ld.so, libc, stack, fork COW), but this breakdown was not measured.

### MapUnit for a Windows platform
- If `MapFile` on Windows (placeholder replace plus `MapViewOfFile3`) costs about the same for a 4 KiB and a 2 MiB window, return **0** like systrap: one fault then maps a whole pma, which minimises debug events. Windows soft faults on an already mapped view are handled in the kernel and never reach the debugger, so there is no reason to under-map.
- If the cost grows with size or placeholders must be split per window, use **2 MiB** (`hostarch.HugePageSize`): it equals the Sentry's own allocation granule for anonymous memory, so it never maps less than the pma the Sentry just allocated, and it is a multiple of the 64 KiB allocation granularity Windows imposes on view base addresses.
- Never go below **64 KiB**: view addresses and file offsets must be 64 KiB aligned (see the ELF survey, gate 0a), and every smaller unit would cost a separate `MapViewOfFile3` per fault.
- `precommit` does not reduce guest-visible faults; at most it avoids a Windows soft fault. It is not needed on the fault path.

## Limits
- Static classification is a linear-sweep heuristic over a 10-instruction window; class 2 vs 3 near basic-block boundaries can be misjudged, and `reloc5` ignores indirect branch targets.
- Dynamic numbers are one run per workload on a shared runner, natively (not under gVisor), with a ptrace tracer. The share figures were stable across the runs made while debugging the workflow (under 1 point of difference).
- Fault counts come from this repository's Linux build of the Sentry with the `sentry_profiling` tag, not the release binary.
