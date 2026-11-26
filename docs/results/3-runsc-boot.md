# 3. runsc boots on Windows

Nodes: "Boot runsc on Windows" (runsc-windows-boot), "Guest terminal and stdio" (sentry-host-stdio-and-tty). Decision: ADR 0017 (refines 0008). Everything ran on `windows-latest` (workflow `.github/workflows/windows-runsc.yml`, branch `runsc-boot`); nothing was run on the dev machine.

CI: https://github.com/lhns/gvisor-windows/actions/runs/45891988954 (green; the erofs step is allowed to fail)

## Design as built
```
runsc.exe run -bundle dir id     (one process; no child, no control server, no gofer, no seccomp)
  runsc/winrunsc          flags (runsc's config flags), OCI bundle (specutils.ReadSpec), Windows overrides
    boot.New(Args)        the real Loader; platform winstub, Windows memory file, windows-nat netstack
    Stdio / Terminal      goroutines between the process's handles and in-Sentry pipes / a devpts pty
    l.RunInProcess(); l.WaitExit(); exit code = guest's (128+signal if killed)
```
- `runsc/main.go` is tagged `!windows`; `runsc/main_windows.go` calls `winrunsc.Main`. `go build ./runsc` for Windows now works (the Linux command side is simply not built). Commands: `run`, `version`. `create/start/state/kill/delete/exec/ps/list/spec/do` are not implemented: they need the sandbox child and its named pipe (ADR 0008), which `run` does not (ADR 0017).
- Loader changes (`runsc/boot`, all marked `gvisor-windows`): `ControllerFD < 0` means no control server (`ctrl` may be nil); no `IgnoreChildStop` on Windows; the gofer FD check and monitor are skipped for a host-folder root; host rlimits are not read; the memory file uses the options of `tools/windows/sentryguest` (`DelayedEvictionManual`, no accounting); `Loader.WindowsStdio` fills fds 0-2 after the mounts exist.
- Config forced on Windows: `--platform=winstub`, seccomp off, directfs off. Root overlay: memory (`--overlay2=none` writes through to the folder). Default `--network` stays the Sentry-only loopback; use `--network=windows-nat --publish=H:G`.
- Root filesystem: a folder (served by the host-folder backend) or, if `root.path` is a file, an erofs image (opened with `rootfs.OpenImage`, passed as the root gofer FD, memory overlay). Files made by Windows tools have no Linux modes: the annotation `dev.gvisor.windows.rootfs-options` = `file-mode=0755,dir-mode=0755,owner=U:G,...` sets the defaults (new `file-mode`/`dir-mode` options).
- Stdio: pumps. stdin handle -> Sentry pipe (closed on EOF; an invalid stdin is EOF), two Sentry pipes -> stdout/stderr handles; after the exit the output pumps are drained (5 s cap), the stdin pump is never waited for. Debug logs go to `--debug-log` only, never to stderr.
- Terminal (`process.terminal` or `-t`): runsc opens `CONIN$`/`CONOUT$`, puts the console in raw VT mode (no echo, line input or Ctrl-C processing; VT input; VT output without newline return), opens `/dev/pts/ptmx` in the guest's devpts, gives the guest the replica as fds 0-2 and as controlling terminal, and pumps console <-> master. Ctrl-C arrives as byte 0x03 and the pty's line discipline sends SIGINT to the foreground process group; the window size is polled every 250 ms and set on the pty (SIGWINCH). New exported helpers in `pkg/sentry/fsimpl/devpts/console.go` (`MasterIndex`, `OpenReplicaWithoutTask`, `SetMasterWindowSize`) because opening the replica and `TIOCSWINSZ` need a task. ConPTY: `CreatePseudoConsole` hosts a *child* of a terminal emulator; runsc is that child, so Windows Terminal, conhost and the CI harness (which creates a pseudoconsole and runs `runsc.exe -t` in it) are the ConPTY side, and runsc needs no ConPTY code.

## fdnotifier
`pkg/fdnotifier` is imported (Windows build of `./runsc`) by: `pkg/sentry/kernel` (only `Pause`/`Resume` around save/restore), `fsimpl/eventfd`, `fsimpl/host`, `fsimpl/gofer` (`special_file.go`), `socket/hostinet`, `socket/unix/transport` (`connectioned.go`), `runsc/boot/portforward`, and the Linux-only device proxies (`nvproxy`, `rdmaproxy`, `tpuproxy/*`). With pumps no host FD is ever registered: nothing in the boot, the guests and the network tests below called `AddFD` (they would have failed with the shim's ENOSYS from `epoll_create`). An IOCP backend is needed only for host-FD passing (`--pass-fd`-style imports, `hostinet`, host Unix sockets, gofer special files); not for stdio, terminals or the NAT.

## What passes (windows-latest, as the freshly created standard user `ci-std`, `run-as-standard-user.ps1`)
`tools/windows/runsctest` writes bundles and runs `runsc.exe run`; busybox and a static glibc hello come from the Linux runner (`busybox-static`), the rootfs is a Windows folder.
| test | result |
|---|---|
| hello (static glibc): stdout, exit code 7 | pass (about 0.5 s per `runsc run`) |
| busybox `sh -c 'echo hi; ls /; cat /etc/hostname; exit 3'` | pass, exit 3 |
| `echo` into stdin of `cat` | pass |
| network: busybox `wget` of a host HTTP server (bound to the runner's non-loopback address) through `windows-nat` | pass |
| published port: guest `httpd` on :8080, `--publish=P:8080`, fetched from Windows at 127.0.0.1:P | pass |
| terminal under a ConPTY the harness creates: shell prompt, `tty` is `/dev/pts/N`, `stty size` 30x100, resize to 40x120 seen by the guest, Ctrl-C interrupts `sleep` (`$?` = 130), `exit 4` gives exit code 4 | pass, also as the runner's administrator |
Hello and the terminal also ran as the administrator (diagnostic steps) to separate privilege from other causes: both pass either way, so the winstub platform needs no admin.

## What fails
- Erofs image as the root (`root.path` is a file): exit 139 (guest SIGSEGV on its first instruction fetch): `Unhandled user fault: addr=4c7c80 ip=4c7c80 access=--x ... winstub: the memory file does not expose sections`. The erofs filesystem maps file pages from the image mapping (`imageMemmapFile`), not from the pgalloc MemoryFile, which the winstub platform requires. Forcing `useReadForIO` on Windows (tried, reverted) moves the failure to `failed to load /bin/sh: function not implemented` (ENOSYS in a shim function on that path; not chased). Needs a change in `fsimpl/erofs` (copy pages into the MemoryFile) or in winstub's `MapFile` (accept another file); the CI step `Erofs image root` runs the case and is allowed to fail. (An image whose files are not owned by uid 0 also fails with EACCES: build with `mkfs.erofs --all-root`.)
- `runsc.exe` accepts only `run`; killing it kills the container.
- Overlay mount options of runc's default spec (`strictatime`, `newinstance`, `ptmxmode`, `mode` on devpts) are logged as unknown and ignored.

## Needed outside `runsc/`
- `pkg/sentry/fsimpl/devpts/console.go` (new, three exported helpers, no behaviour change).
- erofs/winstub: the failure above.
- `windows-nat` has no flag for `HostLoopback`; the network test reaches the host through its non-loopback address instead.

Verdict: `runsc.exe run` on stock Windows boots the real Loader in-process and runs a glibc guest and busybox from a Windows-folder rootfs with stdio, exit codes, stdin, a windows-nat network, a published port and a ConPTY-driven terminal with resize and Ctrl-C, as a standard user; an erofs image root and the create/start/exec command set are still open.
