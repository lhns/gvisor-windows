# runsc's command side: how much is Linux-only

Measured 2026-11-26 on the `go` branch (`GOOS=linux go list -deps ./runsc`), compared with the sandbox graph (`./runsc/boot`).

| | packages | lines |
|---|---|---|
| all of `runsc` | 119 | |
| beyond the sandbox graph | 35 | 42263 |
| of those, importing `x/sys/unix` | 17 | 31003 |

Largest Linux-dependent packages beyond the sandbox: `runsc/cmd` 7617, `runsc/sandbox` 5124, `runsc/container` 4004, `runsc/specutils` 3403, `runsc/cgroup` 3162, `runsc/cmd/sandboxsetup` 1688, `runsc/gofer/fs` 1562, `runsc/cmd/alias/bwrap` 1208.

Verdict: runsc's container-management side adds 35 packages and about 42k lines beyond the sandbox, of which about 31k lines in 17 packages use Linux host calls, mostly process setup, namespaces and cgroups on the host, so the Windows build needs its own sandbox launcher rather than a port of these packages.
