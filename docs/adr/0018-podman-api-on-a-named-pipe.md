# 0018. The Podman API on a named pipe, and Windows paths for bind mounts

- Status: Accepted (decided under the owner's delegation)
- Date: 2026-11-26

## Context
The stock Windows podman client and docker CLI talk to an engine at `npipe:////./pipe/<name>`. The engine (Podman) runs inside the runsc sandbox. The Linux harness already reaches it as a TCP service of the sandbox (`podman system service tcp://0.0.0.0:8888`). Containers started by the engine must be able to bind mount a Windows folder that the client names.

## Decision
- `runsc --pipe-forward=GUESTPORT:PIPENAME` (needs `--network=windows-nat`) serves `\\.\pipe\PIPENAME` and relays every connection to TCP port GUESTPORT of the sandbox, through the NAT's gateway stack (`winnat.DialGuest`), not through a loopback listener, so no other local user can reach the engine. The pipe's security descriptor is `D:P(D;;GA;;;NU)(A;;GA;;;<current user SID>)`: the current user only, network logons denied.
- A guest Unix socket (`/run/podman/podman.sock`) is not forwarded: it would need an in-Sentry dial of a bound socket. The engine listens on a guest TCP port instead.
- `--host-loopback` turns on `winnat.Options.HostLoopback` (the guest reaches the host's 127.0.0.1 services at 10.0.2.2). Off by default, because it exposes every loopback listener of the host.
- Windows folders for bind mounts are mounted into the sandbox WSL-style: `C:\a\b` at `/mnt/c/a/b` (`start-engine.ps1 -Share`), through the host-folder backend. What the client sends for `-v C:\a\b:/w` over a plain `--url`/npipe connection is recorded in `docs/results/5-podman-windows.md`; the translation (client side for machine connections, or a rewrite in the forwarder) is chosen from that result.

## Consequences
- The engine's port is only reachable from the pipe (and from guests on the NAT).
- Only the folders named with `-Share` are visible to the engine; a drive-wide `/mnt/c` is not mounted.
