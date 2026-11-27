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

## Conditions of the pass
The engine is stock Podman 6.2.0 (`quay.io/podman/stable`), started as `podman system service` inside `runsc.exe run` (`tools/windows/podman/start-engine.ps1`, image from `podman-windows.yml`). It differs from a plain `podman system service` run as root on Linux in these settings; each is a property of the missing host, not a change to Podman:
- **Storage driver `vfs`** (image `storage.conf`, `STORAGE_DRIVER=vfs`): the kernel overlay mount of the Sentry, tried on the host-folder storage with fuse-overlayfs unusable, is accepted by `mount` but c/storage's own probe fails (`kernel does not support overlay fs: ... unlinkat .../merged/subdir: directory not empty`, runs 45983747362 and 45984643083, details in `docs/results/5-podman-windows.md`). The Linux harness uses overlay.
  Amended by ADR 0021: the default is now `overlay`; `vfs` stays selectable.
- **Storage on a Windows folder** (`/var/lib/containers` bind mount, `case-sensitive`): so images, volumes and the database survive a restart; a Windows host has no Linux filesystem for it.
- **Marker file `/etc/containers/podman-machine` and `gateway.containers.internal` in `/etc/hosts`**: Podman exposes published ports only through a machine gateway; the windows-nat gateway (10.0.2.2) plays that role.
- **Listener `tcp://0.0.0.0:8888` relayed to the pipe `\.\pipe\podman-ci` and a unix socket** (`--pipe-forward`, `--unix-forward`): the engine cannot listen on a Windows pipe; the Windows podman client dials the unix socket, the docker CLI the pipe.
- **`containers.conf` and `storage.conf` from the harness** (cgroupfs manager, `cgroups = "disabled"`, file events, k8s-file logs, crun): the sandbox has no systemd or cgroup controllers; the same files as the Linux harness.
- **Mounts in the spec** (proc, sysfs, cgroup2, devpts, tmpfs on `/dev`, `/dev/shm`, `/run`, `/tmp`, `/var/tmp`, and the device nodes): docker or systemd would provide them. `/var/tmp` is a tmpfs because buildah mounts an overlay with its upperdir there and the Sentry takes only tmpfs or host-folder uppers for user mounts.
- **`/etc/resolv.conf` with `10.0.2.3`**: the Sentry writes none; that is the virtual DNS of windows-nat.
- **All 41 capabilities** (`docker run --cap-add all` of the Linux harness) and `CONTAINERS_STORAGE_CONF=/etc/containers/storage.conf` (ignores the image's fuse-overlayfs drop-ins, as the Linux harness).
- **runsc flags** `--overlay2=root:memory` (the image root is read-only; the engine writes to it), `--in-sandbox-cgroup=v2` (cgroupfs manager needs a cgroup2 tree), `--network=windows-nat`. Diagnostics only: `--debug`, `--log-level=info`, hang dump.
- **Graceful stop**: the harness sends SIGTERM through `runsc kill` before killing, as `docker stop` would.

Removed since the first pass: `GODEBUG` for AVX-512, `BUILDAH_ISOLATION=oci` (the Sentry reported the initial user namespace with a different inode number than Linux's `USER_NS_INIT_INO`, so c/storage's `IsRootless()` was true), the tmpfs for the sqlite database, the reduced capability list, `CONTAINERS_CONF`.
