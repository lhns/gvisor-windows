# 0009. Running gVisor's tests against the port

- Status: Accepted (decided under the owner's delegation)
- Date: 2026-11-26

## Context
The `go` branch ships no `_test.go` files; upstream's tests and the syscall test suite (`test/syscalls`, C++ built with Bazel) live on `master`.

## Decision
- Development stays on the `go`-based branch (ADR 0003).
- Unit tests for packages we port are copied from the matching `master` commit into the package (`_test.go`, unchanged where possible) and run with `go test` on Windows CI.
- The syscall test suite is built as static Linux binaries from `master` with Bazel in Linux CI, uploaded as an artifact and run as guests under the Windows port; its pass rate is published.

## Consequences
- No Bazel on Windows; the Go toolchain is enough for the port itself.
- Copied tests are refreshed when upstream is merged.
