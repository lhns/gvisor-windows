# Phase 1f: dynamic programs, exec, pipes, signals and threads under the Sentry

Nodes: "Dynamic programs, exec and pipes" (sentry-dynamic-elf-and-exec), "Guest threads and signals" (sentry-threads-and-signals). Everything ran on `windows-latest` (`windows-sentry-guests.yml`, branch `sentry-exec`); nothing was run on the dev machine.

CI: https://github.com/lhns/gvisor-windows/actions/runs/45886949491 (all 28 cases pass: the 8 static guests of 1d and 20 new ones). No change to `pkg/` was needed.

## Harness and guests
- `sentryguest -tar root.tar [-env PATH=...] /path/in/tar args...` fills the in-memory tmpfs from a tar (directories, files, symlinks, hard links) and starts the program at that path with argv[0] as given, so interpreters, shared libraries and `/bin/sh` are found as in a root filesystem. No host files are used; stdio is still one tmpfs file shared by all processes. Default environment `PATH=/bin:/usr/bin:/`, `HOME=/`.
- The Linux job builds the guests and a root filesystem tar (`rootfs.tar`: busybox-static from Ubuntu with applet symlinks, glibc 2.39 interpreter and libraries copied via `ldd`, alpine 3.20 `ld-musl`, a small `libplug.so`, two shell scripts). Sources: `test/guests/{dlopen,plug,exec,signals,threads}.c`, `gostress/`, `t/*.sh`; `hello.c` is reused for the dynamic glibc and musl hello.
- The guest is pid 1 of its pid namespace and has no `/proc`, `/dev` or `/sys`.

## What passes
| area | case | what it checks |
|---|---|---|
| dynamic ELF | dyn_glibc, dyn_glibc_nopie | PIE and non-PIE glibc via `ld-linux-x86-64.so.2` + `libc.so.6`, stack protector, TLS, malloc |
| | dyn_musl | alpine musl (`ld-musl`, libc is the interpreter) |
| | dlopen_glibc | `dlopen` of an own `.so` (constructor, `__thread` variable in a library, dlsym) and of `libm.so.6`, `dlclose` |
| exec | exec_chain | static program execs itself three times: argv (renamed argv[0]), envp replaced each time, auxv (PAGESZ, PHDR/PHNUM/PHENT, RANDOM, UID/EUID/SECURE, SYSINFO_EHDR, CLKTCK, PLATFORM, EXECFN) |
| | fork_exec | fork + execve with argv/envp, exit status 9, execve ENOENT in a child, three children reaped with `wait`, then ECHILD |
| pipes and shell | sh_pipes | `sh -c 'echo a \| cat; echo $((1+2)); for i in 1 2 3; do echo $i; done \| wc -l'` |
| | sh_pipeline_tools | `printf \| sort \| uniq \| grep -v \| sed` (5 processes) |
| | sh_wait_status | `$?` of exit 3, of a shell killed by SIGTERM (143), of a killed background job (`wait`), of a pipeline |
| signals | sig_self | `kill` to self: handler runs before `kill` returns, xmm5 survives the handler, blocked signal stays pending and is delivered on unblock |
| | sig_alarm | `alarm` + `pause`, `setitimer` 20 ms periodic |
| | sig_segv | SEGV handler recovering with `siglongjmp` 3 times, `si_addr` NULL, `PROT_NONE` page gives `SEGV_ACCERR`, no fault after `mprotect` |
| | sig_altstack | `sigaltstack` + `SA_ONSTACK`: stack overflow (infinite recursion) handled on the alternate stack |
| | sig_ctx | handler edits `uc_mcontext` (rax, r12 of an interrupted `syscall`; rip and rax of a faulting store): `sigreturn` restores the modified registers |
| | sig_eintr | `pthread_kill` to the main thread blocked in `nanosleep` (EINTR, remaining time) and in a raw futex wait (EINTR); with `SA_RESTART` a blocked `read` on a pipe continues after two handlers and gets its byte |
| | sig_child | SIGCHLD handler with `waitpid`, `sigsuspend`, parent `kill`s a child (SIGTERM: `WIFSIGNALED`; SIGUSR1 handler in the child: exit 5), SIGSTOP/SIGCONT with `WUNTRACED`/`WCONTINUED`, SIGKILL |
| | sig_group | `setpgid`, `getpgid`, `kill(-pgid)` kills child and grandchild, grandchild reparented to the pid-1 guest and reaped, then ESRCH for the group |
| threads | threads_static, threads_dyn | 8 pthreads: mutex (160000 lock rounds), condvar barrier, per-thread TLS (also a 4 KB `__thread` array), `pthread_exit` and return values, join |
| | go_stress | Go, `GOMAXPROCS=4`: 6 CPU-bound goroutines in a call-free loop (only stoppable by asynchronous preemption, SIGURG), a 5 ms ticker that must tick 20 times meanwhile, a 50 ms timer, a pipeline of 8 channel stages passing 1000 values |

Output and exit status are compared exactly by the harness for every case.

## Things that did not pass at first (all test or expectation errors, none a Sentry or platform defect)
- `errno == ECHILD` evaluated before `wait()` in one `printf` call (unspecified argument order).
- busybox `ash` prints `Terminated` for a child killed by SIGTERM, as on Linux; expectation corrected.
- `threads_dyn`: glibc's `pthread_exit` needs `libgcc_s.so.1`; added to the tar.
- Races with the process start time (a `fork` takes 0.3 to 1 s here): a kick that arrives while the main thread is still inside `fork` coalesces with the next one; a parent that signals a child before the child installed its handler kills it by default action; a group kill 150 ms after `fork` came before the grandchild existed. The guests now wait on pipes, or for 2 s in `sig_group`.

## Numbers (run time from `CreateProcess` to exit, noisy 4 vCPU runner; two runs differed by up to 1.5x)
- Dynamic glibc hello 0.56 s, musl 0.47 s, dlopen 0.63 s, against 0.31 to 0.49 s for the static glibc hello: the interpreter, a few more `MapFile` ranges and library text patching cost roughly 0.1 to 0.2 s.
- A new process costs 0.9 to 1.2 s inside a guest (exec chain of 3: 1.16 s; fork+exec+3 more forks: 2.6 s; the busybox pipelines with 4 to 5 processes: 3.3 to 3.8 s). This is the per-process start cost of the platform (a new stub process per address space, also on `execve`), now the dominant cost of shell-heavy work; the platform owner is reducing it.
- Signal delivery itself is cheap: `sig_self`, `sig_segv`, `sig_ctx` take 0.41 to 0.43 s in total, the same as a hello.

## Platform bugs for the platform owner
None found. Open observations only:
- No `/proc/self/exe`, so programs that re-exec themselves through it (busybox with `FEATURE_PREFER_APPLETS` builds) were not exercised; the Ubuntu build execs through `PATH`.
- Not covered: `ptrace`, `SA_SIGINFO` with `si_addr` for non-canonical faults, `vfork`, signal delivery while a thread is inside the platform's repair path (the Go test runs about 4 s of SIGURG preemption, 20 timer ticks and 6 spinners with no hang or crash, but a rare race would not show in one run).

## What this does not show
- The shell and the pipes run on the in-memory tmpfs with stdio in one file, not through the `runsc` boot path, a host terminal or host pipes.
- Only one glibc (2.39) and one musl (1.2.5) version; no `ld.so.cache`, no `/etc`, no locale or NSS.

Verdict: PASS. Under the real Sentry on `windows-latest`, dynamic glibc and musl programs with `dlopen`, execve chains, fork+exec+wait, busybox shell pipelines, handled and fatal signals (alarm, segv with recovery, sigaltstack, modified `sigreturn` context, EINTR and restart of blocked syscalls, SIGCHLD, stop/continue, process groups), 8 pthreads and a Go program with preemption at GOMAXPROCS=4 all give the exact expected output, with no Sentry or platform change; the remaining cost is about one second per new process.
