# 0006. A Linux-ABI shim for x/sys/unix on Windows

- Status: Accepted
- Date: 2026-11-26

## Context
Type-checking the sandbox (`./runsc/boot`) for `GOOS=windows` gives about 2,200 errors in 246 packages; almost all are names from `golang.org/x/sys/unix`, which has no Windows files. Of 3,189 `unix.` references, about 2,190 are constants (errnos, `O_*`, `PROT_*`, `SYS_*`), about 290 are types (`Errno`, `Iovec`, `Stat_t`, `Timespec`, ...) and about 710 are calls. The constants and types are mostly used as the *guest's* Linux ABI, so their Linux values are correct on Windows too. Runtime `go:linkname` uses (`gopark`, `goready`, `nanotime`, `memmove`, ...) are OS-independent; the fork-related ones sit only in the Linux platforms.

## Decision
Make the names resolve instead of editing every file: a `replace` of `golang.org/x/sys` with an in-tree copy (`third_party/golang.org/x/sys`, same version) that adds `unix/*_windows.go`, defining the Linux constants and types for `windows/amd64` and the functions as stubs returning `ENOSYS`. Real Windows behaviour is implemented where it matters (host fds, memory, events, sockets, files), in `_windows.go` files of the packages that need it. Linux-only subsystems (KVM/systrap/ptrace platforms, nvproxy, tpuproxy, rdmaproxy, seccomp filters, cgroup setup) are excluded from the Windows build with build tags.

## Consequences
- The Sentry compiles for Windows with few edits to shared code, which keeps upstream merges small.
- A stubbed call fails at run time with `ENOSYS`, not at compile time; each milestone's tests are what find them.
- The standard library's `syscall` errno constants differ on Windows; code that compares them with Linux values must use `unix` or `linuxerr`. This is checked when porting each package.
