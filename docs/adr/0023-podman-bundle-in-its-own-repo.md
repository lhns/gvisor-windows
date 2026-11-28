# 0023. The Podman bundle lives in its own repository

- Status: Accepted (owner decision, 2026-11-28)
- Date: 2026-11-28
- Refines: ADR 0002 (the acceptance test stays here)

## Context
CI used to build a zip with `runsc.exe`, the Podman engine image, `podman-engine.ps1` and a README (`release` and `release-test` jobs in `ci.yml`). That is a product built on gVisor, not part of the port: it has its own versioning (Podman releases), its own packaging plans (a single-binary launcher, a Linux bundle) and its own users.

## Decision
- The bundle moves to https://github.com/lhns/podman-portable. `podman-engine.ps1` and its README moved there; this repo no longer builds or ships them.
- gvisor-windows stays gVisor-shaped and releases only `runsc` (Windows and Linux) and a hello bundle, through `release.yml`.
- The dependency is one way: podman-portable pins a gvisor-windows commit or release. gvisor-windows CI never references podman-portable.
- This repo keeps the bar from ADR 0002: `test/e2e/podman-acceptance`, `start-engine.ps1`, `ci-run.ps1`, `build-image.sh`, the `podman-image` action and `podman-windows.yml`. A new Podman version must pass this acceptance here before podman-portable ships it.

## Consequences
- A change to the engine start-up that podman-portable also needs is made in `start-engine.ps1` here and picked up by pinning the new commit.
- This repo's clean-machine check (`release-test`) covers only runsc and the hello bundle; the end-user experience of the Podman bundle is tested in podman-portable.
