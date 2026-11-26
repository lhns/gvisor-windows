# 0008. runsc on Windows: commands and process layout

- Status: Accepted (decided under the owner's delegation); process layout refined by ADR 0017 (run boots in-process for now)
- Date: 2026-11-26

## Context
runsc's command side is about 42k lines beyond the sandbox, about 31k Linux-specific (namespaces, cgroups, seccomp, gofer processes on the host). On Windows there are no host namespaces to set up, and files are opened directly by the Sentry (ADR 0005).

## Decision
- The Windows build keeps runsc's OCI interface and supports first: `run`, `create`, `start`, `state`, `kill`, `delete`, `exec`, `ps`, `list`, `spec` and `do`. Checkpoint/restore, GPU proxies and `install` are out of scope.
- Process layout: `runsc.exe` starts one sandbox process (the Sentry) per sandbox; the Sentry starts one stub process per guest address space. There is no gofer process.
- Control and I/O between `runsc.exe` and the sandbox use named pipes; container state lives under `%LOCALAPPDATA%\runsc`.
- A new Windows sandbox launcher replaces `runsc/sandbox`/`container` setup code instead of porting its Linux namespace logic.

## Consequences
- runsc's flags, OCI bundles and state format stay the same, so tooling that drives runsc keeps working.
- Linux-only command code is excluded from the Windows build by build tags.
