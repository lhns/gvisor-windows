# 0004. Phase 0 results: go/no-go

- Status: Accepted (go: the owner asked for the full project on 2026-11-26)
- Date: 2026-11-26

## Context
Phase 0 tested whether porting gVisor to Windows hosts is feasible (ADR 0001) and how far stock gVisor is from the acceptance bar (ADR 0002). Details: `docs/results/0a-linux.md`, `docs/results/0b-windows.md`, `docs/results/runsc-command-side.md`.

| gate | result |
|---|---|
| S0 low addresses | standard users cannot map below 0x10000: zpoline is out, syscall sites are rewritten to traps or jumps |
| S1 trap cost | `ud2` + vectored handler 1.7 µs (hosted runner) to 8 µs (dev VM) per trap; cross-process spin round trip 0.13-0.29 µs, event round trip 24-55 µs |
| S2 4 KB views | with placeholders, section views map at 4 KB addresses and offsets into another process, confirmed on the dev machine (10.0.26300) and the hosted runner; without placeholders this fails (error 1132); one placeholder split plus one map call per region |
| S3 TLS register | `wrfsbase` works from user mode on FSGSBASE CPUs; Windows resets the base on every deschedule; a vectored handler restores it transparently (0 wrong reads, 1.1-2.2 µs per repair); fails on CPUs/VMs without FSGSBASE |
| S4 restricted stub | stub runs with a low-integrity restricted token under full parent control; a missed syscall still runs a real NT service |
| S5 Go fault recovery | access violations in Go code and assembly recover (SetPanicOnFault or a RIP-fixup handler), about 1.6-6 µs per fault |
| S6 WHPX | not available on the dev machine as a standard user |
| Podman on stock runsc (Linux) | Podman 5.7 (not 6.x) with `cgroups=disabled` in containers.conf, host networking, store on a host directory and `--in-sandbox-cgroup=v2`: all 10 steps pass, but steps 7-8 only in a host-network variant (nginx on the host port, compose over 127.0.0.1); bridge networking fails (netavark netlink); `-p` and compose service names were never exercised; cgroup v2 limits inside the sandbox are untested |
| Benchmarks (Linux) | runsc vs runc: fork+exec about 5x, small files on a bind mount 2-3x, CPU 1.1-1.4x |
| ELF survey | 73-82% of ELF files have segments not 64 KB-aligned (moot given S2) |
| Image storage | a store on a host directory (Linux host filesystem) survives restarts with both the overlay and vfs drivers; no `--overlay2` medium persists |
| Process cost (not measured) | every guest address space is a Windows process, and fork+exec is already about 5x native inside gVisor on Linux; the cost of creating a stub and of one map call per region is the open speed risk |
| runsc command side | 35 packages / about 42k lines beyond the sandbox, about 31k Linux-specific: Windows needs its own sandbox launcher |

## Decision
**Go**, with these conditions:
- Native execution requires a CPU with FSGSBASE, which Windows lets user mode use; machines or VMs without it (such as the QEMU-guest dev machine) are unsupported unless a later fallback rewrites `%fs` accesses. Only GitHub's hosted Windows image was tested; the S3 binary (artifact of the `windows-spikes` workflow) should also run on a real target machine before relying on this.
- The stub keeps a fault-and-repair handler for the FS base, and syscall hand-off uses spinning plus patched hot sites, since a trap per syscall costs 1.7-8 µs.
- The first number of Phase 1 is the cost of creating a stub process and mapping its memory.
- Bridge networking and port publishing are gVisor gaps to fix or work around.

Follow-up decisions, taken under the owner's delegation:
- Persistent storage: ADR 0007 (real files in a Windows folder; the Linux-filesystem result does not yet show NTFS can carry the metadata).
- The bar is the strict one of ADR 0002: `-p` and compose service names are required; host networking is an interim state.

## Consequences
- Phase 1 starts with the stub/controller prototype, the FS-base repair handler and the Sentry building for Windows.
- ADR 0001 becomes Accepted on a go; on a no-go it is superseded.
- The development machine cannot run native guests; Windows work is tested on hosted runners or FSGSBASE hardware.
