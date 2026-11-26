# 0001. Port gVisor to Windows hosts

- Status: Proposed (decided after Phase 0, ADR 0004)
- Date: 2026-11-26

## Context
gVisor runs only on Linux hosts. Goal: gVisor (`runsc` and the Sentry) running on Windows for a standard user, without admin rights, Hyper-V, WSL or a VM, with guest x86-64 code executing natively on the CPU. Other ways to run Linux code on Windows were examined first:

| candidate | verdict |
|---|---|
| gVisor Sentry | User-space Linux kernel that already runs OCI containers (namespaces, cgroup v2, overlay, netstack). Linux hosts only, but the host side sits behind a pluggable platform interface. |
| LKL / nommu UML | Real Linux kernel, but nommu mode has no `fork()` and no memory protection between processes; LKL dropped Windows hosts in 2020; nommu UML is unmerged (v14, Feb 2026). |
| unkernel | Runs Linux compiled to wasm off Linux today, but guest code is always emulated (aarch64 only), so it cannot run x86 user code natively. |
| flinux / WSL1-style translation | Translates Linux syscalls onto Windows calls; has no namespaces, cgroups or overlay, so it cannot host a container engine without writing a kernel. |
| QEMU (TCG, optionally WHPX) | Works today, but software emulation of the whole machine is several times slower; WHPX needs a one-time admin step. |

Measured on the `go` branch, sandbox side only: the `runsc/boot` dependency graph is 265 packages (~585k lines); 88 packages import `x/sys/unix`. The Sentry does copy-on-write itself (`pkg/sentry/mm`) and reads guest memory through its own mappings, so a platform only needs address-space mapping, context switching and syscall interception.

## Decision
Port gVisor to Windows hosts: a new platform (stub processes, syscall trap, memory mapping) and Windows host layers (memory file, events, network, files), keeping gVisor's own interface, `runsc`.

## Consequences
- Linux syscall semantics come from gVisor; the new work is the Windows platform, the host layers and the Windows side of `runsc`.
- Go/no-go risks tested in Phase 0: setting the `%fs` TLS base from user mode, trap cost, 64 KB allocation granularity, fault recovery in Go on Windows.
- Open: the container-management side of `runsc` (`runsc/cmd`, `container`, `sandbox`, `gofer`, `cli`) was not measured, and which `runsc` commands the Windows build supports is not decided yet.
- The rejected candidates stay recorded in the tech tree.
