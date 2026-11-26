# 0002. Podman's engine inside the sandbox

- Status: Proposed (decided after Phase 0, ADR 0004)
- Date: 2026-11-26

## Context
Podman's engine (libpod, crun, netavark, containers/storage) is Linux-only. On Windows, Podman normally ships it inside a VM (`podman machine`), which needs WSL or Hyper-V.

## Decision
Run Podman's unmodified Linux engine and crun inside one gVisor sandbox. Windows gets only the stock remote client (`podman`/`docker` CLI) over a named pipe `\.\pipe\podman-<name>`. `podman machine` is not used.

## Consequences
- The goal is defined by one acceptance test (`test/e2e/podman-acceptance`): pull, run, build, bind mounts, ports, compose, persistence, isolation, from a standard Windows user.
- The same test runs first on Linux against Podman inside runsc; failures there are gVisor limits, not porting gaps.
- If Podman cannot run inside gVisor (Phase 0a), this ADR is superseded by a runsc-per-container design with a different engine.
