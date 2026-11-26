# 0002. What done means

- Status: Accepted (owner decision)
- Date: 2026-11-26

## Context
"Enough of the Linux interface works on Windows" needs a measurable bar, not a list of syscalls. The project itself is gVisor on Windows hosts (ADR 0001), not a container engine.

## Decision
The port is done when an unmodified Podman engine passes the acceptance test `test/e2e/podman-acceptance` while running on gVisor on Windows, for a standard user. The test covers pull, run, interactive TTY, build, bind mounts of Windows folders, published ports, compose, persistence across restarts and isolation between containers. How the test hosts Podman on gVisor is a harness detail.

## Consequences
- The same test first runs on Linux against Podman on stock `runsc`. Steps that fail there are gaps in gVisor itself, recorded as such, not limits of the port.
- Podman is a workload here; the project's interface remains `runsc`.
