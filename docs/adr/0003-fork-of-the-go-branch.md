# 0003. A fork of gVisor's go branch

- Status: Accepted
- Date: 2026-11-26

## Context
gVisor's `go` branch is a synthetic branch buildable with standard Go tools; `master` needs Bazel. Upstream changes must stay easy to take in.

## Decision
- Repo `lhns/gvisor-windows`, an unofficial port, not a Google project; work happens on branch `windows`, based on upstream `go`.
- Windows code lives in new packages and `_windows.go` files; edits to shared code stay minimal and are marked.
- Code borrowed from other projects carries its origin in `NOTICE` and a comment; licences are reviewed later by the owner.
- Fixes that would also help upstream stay on our branches; only the owner opens pull requests or issues upstream.

## Consequences
- Upstream is merged in regularly from `upstream/go`.
- The `go` branch has no `test/syscalls`; whether to run gVisor's syscall tests from a `master` checkout is decided after Phase 0.
- Linux-side gates and tests run on GitHub Actions (`ubuntu-latest`); Windows-side spikes run on a standard-user Windows machine.
