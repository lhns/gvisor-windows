# runsc for Windows

Unofficial port of gVisor (https://gvisor.dev) to Windows hosts. Not a Google project. See NOTICE and LICENSE.

**Not a security sandbox on Windows.** The port runs Linux code natively and catches its system calls by rewriting them; code that jumps into the middle of an instruction can still reach the Windows kernel, and host folders are opened with your own rights. Use it as a compatibility layer, not as isolation.

## Try it
Extract the zip anywhere (a standard user is enough, no admin rights), then in PowerShell:

    .\runsc.exe --network=none run -bundle bundle hello

It prints a line from a Linux program and exits with code 7. `bundle\` is an OCI bundle whose root folder holds a `hello` program and busybox (`sh`, `ls`, `cat`, `echo`); edit `process.args` in `bundle\config.json` to run a shell command, for example `["/bin/sh","-c","echo hi"]`.

For a ready-to-run Podman engine see https://github.com/lhns/podman-portable.
