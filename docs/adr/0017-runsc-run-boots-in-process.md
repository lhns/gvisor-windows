# 0017. `runsc run` boots the Sentry in the runsc process

- Status: Accepted (decided under the owner's delegation)
- Date: 2026-11-26
- Refines: ADR 0008

## Context
ADR 0008 plans one sandbox process per sandbox, with named pipes for control and I/O. `runsc run` is the one command that lives exactly as long as its container, and it has to pass the container's stdio, console and exit code through, which are the runsc process's own Windows handles. The Linux boot path is a control server on a socket, host FDs for stdio and gofer FDs; none of it exists on Windows.

## Decision
- `runsc.exe` on Windows is a separate small command line (`runsc/winrunsc`, `runsc/main_windows.go`). The Linux command side (`runsc/cmd`, `container`, `sandbox`) is not built for Windows.
- `run` creates the `boot.Loader` in the same process and runs the container there: no child process, no control server (`Args.ControllerFD < 0`), no seccomp, no gofer. The loader is started through one function, so the later commands (`create`, `start`, `exec`, `kill`) can run the same code in a sandbox child that is controlled through a named pipe.
- Stdio is not host FDs: goroutines pump the process's stdin, stdout and stderr handles into in-Sentry pipes (`boot.Stdio`). With `process.terminal`, the console of the process is put in raw VT mode and bridged to a Sentry pty; its resize is polled. A ConPTY is only needed by a host that has no console (a test harness, a service); Windows Terminal and conhost already are one.
- The exit code of `runsc run` is the container's: its exit status, or 128 plus the signal.

## Consequences
- `run` works with the same flags and OCI bundles as on Linux, apart from the Windows-only `--network=windows-nat`, `--publish` and the rootfs folder options (`dev.gvisor.windows.rootfs-options`).
- Without a sandbox child, killing `runsc.exe` kills the container; there is no `state` or `kill` of a running container yet.
- No host FD is imported into the Sentry, so `pkg/fdnotifier` is never asked to poll one.
