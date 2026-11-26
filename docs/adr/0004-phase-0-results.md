# 0004. Phase 0 results: go/no-go

- Status: Proposed (the owner decides go or no-go)
- Date: 2026-11-26

## Context
Phase 0 tested whether porting gVisor to Windows hosts is feasible (ADR 0001) and how far stock gVisor is from the acceptance bar (ADR 0002). Details: `docs/results/0a-linux.md`, `docs/results/0b-windows.md`, `docs/results/runsc-command-side.md`.

| gate | result |
|---|---|
| S0 low addresses | standard users cannot map below 0x10000: zpoline is out, syscall sites are rewritten to traps or jumps |
| S1 trap cost | `ud2` + vectored handler 1.7 µs (hosted runner) to 8 µs (dev VM) per trap; cross-process spin round trip 0.13-0.29 µs, event round trip 24-55 µs |
| S2 4 KB views | with placeholders, section views map at 4 KB addresses and offsets into another process; no 64 KB workaround needed on 10.0.26300 |
| S3 TLS register | `wrfsbase` works from user mode on FSGSBASE CPUs; Windows resets the base on every deschedule; a vectored handler restores it transparently (0 wrong reads, 1.1-2.2 µs per repair); fails on CPUs/VMs without FSGSBASE |
| S4 restricted stub | stub runs with a low-integrity restricted token under full parent control; a missed syscall still runs a real NT service |
| S5 Go fault recovery | access violations in Go code and assembly recover (SetPanicOnFault or a RIP-fixup handler), about 1.6-6 µs per fault |
| S6 WHPX | not available on the dev machine as a standard user |
| Podman on stock runsc (Linux) | all 10 acceptance steps pass with host networking, store on a host directory, `--in-sandbox-cgroup=v2`; bridge networking fails (netavark netlink), `-p` unverified |
| Benchmarks (Linux) | runsc vs runc: fork+exec about 5x, small files on a bind mount 2-3x, CPU 1.1-1.4x |
| ELF survey | 73-82% of ELF files have segments not 64 KB-aligned (moot given S2) |
| Image storage | a store on a host directory survives restarts; no `--overlay2` medium persists |
| runsc command side | 35 packages / about 42k lines beyond the sandbox, about 31k Linux-specific: Windows needs its own sandbox launcher |

## Decision
Recommendation: **go**, with these conditions:
- Native execution requires a CPU with FSGSBASE, which Windows lets user mode use; machines or VMs without it (such as the QEMU-guest dev machine) are unsupported unless a later fallback rewrites `%fs` accesses.
- The stub keeps a fault-and-repair handler for the FS base, and syscall hand-off uses spinning plus patched hot sites, since a trap per syscall costs 1.7-8 µs.
- Persistent storage uses real files in a Windows directory (option (a) of R3), backed by the measured host-directory result.
- Bridge networking and port publishing are gVisor gaps to fix or work around; the acceptance test runs with host networking until then.

## Consequences
- Phase 1 starts with the stub/controller prototype, the FS-base repair handler and the Sentry building for Windows.
- ADR 0001 becomes Accepted on a go; on a no-go it is superseded.
- The development machine cannot run native guests; Windows work is tested on hosted runners or FSGSBASE hardware.
