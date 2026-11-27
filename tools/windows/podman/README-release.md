# gvisor-windows: portable build

Unofficial gVisor port to Windows: `runsc.exe`, plus a Podman engine that runs inside it. Built by CI from one commit
(the zip name has its short sha). Source and decisions: https://github.com/lhns/gvisor-windows

**Not a security sandbox on Windows.** The port runs Linux code natively and catches its system calls by rewriting them; code that jumps into the middle of an instruction can still reach the Windows kernel, and host folders are opened with the user's own rights. Treat it as a compatibility layer, not an isolation boundary.

## Requirements

- 64-bit Windows 10 or later (x64). No administrator rights, no WSL, no Hyper-V.
- Windows PowerShell 5.1 (built in) or PowerShell 7.
- Internet access for pulling images; a stock Podman remote client for Windows (`podman-remote-release-windows_amd64.zip`
  from https://github.com/containers/podman/releases, the engine is Podman 6.2.0) or a Docker CLI.
- About 2 GB of disk for the extracted zip and the first images.

Extract the zip to any folder you own (for example `C:\Users\you\gvisor-windows`); the commands below run from there.
CPUs without FSGSBASE or XSAVE use a slower rewrite mode automatically.

## Try runsc.exe

    .\runsc.exe --network=none run -bundle bundle hello

Runs a static Linux program that prints a line and exits with 7 (`$LASTEXITCODE` is 7).

## Start the engine

    powershell -NoProfile -ExecutionPolicy Bypass -File .\podman-engine.ps1 start

The engine (Podman inside runsc.exe) listens on the named pipe `\\.\pipe\podman-gvisor` and on a unix socket in the data
folder, both for the current user only. It answers about a minute after the start. `status` shows both ways to connect.
Pass `-Share D:\work` to let containers bind-mount from that folder (it appears as `/mnt/d/work`).

## Use stock clients

Podman (the client cannot dial pipes, it uses the socket; `status` prints the exact line for your account):

    podman system connection add --default gvisor unix:///Users/you/AppData/Local/gvisor-windows/engine/sock/podman.sock
    podman version
    podman run --rm alpine echo ok

Docker CLI (the pipe):

    $env:DOCKER_HOST = 'npipe:////./pipe/podman-gvisor'
    docker run --rm alpine echo ok

## Stop

    powershell -NoProfile -ExecutionPolicy Bypass -File .\podman-engine.ps1 stop

It sends SIGTERM to the engine so Podman closes its store, and kills the sandbox only if it does not exit within 60 s.
Images and volumes stay and are there at the next start.

## Where data lives

`%LOCALAPPDATA%\gvisor-windows` (`-Data <folder>` changes it): `engine\storage` (images, containers, volumes),
`engine\logs`, `engine\sock`. Delete the folder, with the engine stopped, to start from scratch.

## Conditions of the Podman pass

The Podman acceptance (pull, run, TTY, build, bind mounts, ports, compose, restart, isolation) passes as a standard user
under these conditions, each a property of the missing Linux host and not a change to Podman: stock Podman 6.2.0 from
`quay.io/podman/stable`; storage driver `overlay` on the Sentry's overlay (`-StorageDriver vfs` selects vfs; docs/adr/0021); storage on a Windows
folder; published ports through the windows-nat gateway (a `podman-machine` marker in the image); the engine listens on
TCP 8888 inside the sandbox and runsc relays it to the pipe and socket; cgroupfs manager with cgroups disabled (no
systemd); the harness's containers.conf and storage.conf; all capabilities; `--network=windows-nat`,
`--overlay2=root:memory`, `--in-sandbox-cgroup=v2`. Details: docs/adr/0002 (Conditions of the pass).
