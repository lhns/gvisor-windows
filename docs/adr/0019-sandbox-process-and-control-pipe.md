# 0019. A sandbox process with a control pipe for the container lifecycle

- Status: Accepted (decided under the owner's delegation)
- Date: 2026-11-27
- Refines: ADR 0008, 0017

## Context
`runsc run` keeps the Sentry in the runsc process (ADR 0017). The OCI runtime interface also needs `create`, `start`, `state`, `kill`, `delete`, `exec`, `ps` and `list`, where the container outlives the command that made it.

## Decision
- `create` starts a detached copy of `runsc.exe` with the hidden command `sandbox` (same global flags, stdout and stderr to `<root>\<id>\stdout.log` and `stderr.log`, stdin NUL, outside the creator's job object where allowed). It boots the Loader with the container not started, and serves the control pipe `\\.\pipe\runsc-<id>` with the security descriptor of `--pipe-forward` (current user only, network logons denied). `create` returns when the pipe answers.
- The pipe speaks one JSON request per connection (`state`, `start`, `kill`, `ps`, `wait`); `exec` turns the connection into a stream of JSON frames (stdin, stdout, stderr, exit code), because exec's stdio cannot be Windows handles passed to another process. `exec` runs a process in the container with in-Sentry pipes and the same pumps as `run`.
- State is `<root>\<id>\state.json` (runc's state fields plus the sandbox pid and the exit code), root = `--root`, default `%LOCALAPPDATA%\runsc` (ADR 0008). `state`, `list` and `delete` work from the file; "created" or "running" with a dead sandbox pid is reported as "stopped". The sandbox writes "stopped" and the exit code itself, lingers 2 s for a pending `wait`, then exits.
- Not supported: a container with `process.terminal` through `create`/`start` (no console to attach; use `run`), `pause`, `resume`, `events`, `checkpoint`, `restore`, `spec`, `do`.

## Consequences
- Container ids are limited to letters, digits, `-`, `_` and `.`.
- `delete -f` terminates the sandbox process; its Windows resources go with it.
- The container's stdio is in log files; a runtime that wants the container's stdout (Podman via conmon) needs a different arrangement, not designed here.
