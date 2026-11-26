# Roadmap

Everything between the GENERATED markers is rebuilt from `roadmap/nodes.yaml`; edit that file, not the regions. Text outside the markers is kept.

## Legend

<!-- BEGIN GENERATED: legend -->
```mermaid
flowchart TB
    %% legend
    subgraph lg_g0["Done: colour is the rating"]
        direction LR
        lg_s0["✔ very successful"]
        lg_s1["✔ successful, or fixed"]
        lg_s2["✔ mixed"]
        lg_s3["✔ bad (regression)"]
        lg_s4["✔ done, not rated"]
    end
    subgraph lg_g1["Closed"]
        direction LR
        lg_s5["✖ dead end"]
        lg_s6["↷ superseded"]
    end
    subgraph lg_g2["Open: colour is the state"]
        direction LR
        lg_s7["▶ in progress"]
        lg_s8["⏳ built, waiting"]
        lg_s9["○ next"]
        lg_s10["⛔ blocked"]
        lg_s11["⏸ parked"]
        lg_s12>"⚠ open, never rated"]
    end
    subgraph lg_shape["Shape: kind"]
        direction LR
        lg_k0["○ feature"]
        lg_k1[/"○ experiment"/]
        lg_k2["○ tooling"]
        lg_k3>"⚠ defect"]
        lg_k4{{"○ owner-decision<br/>(open, not drawn)"}}
        lg_k5{{"✔ owner-decision<br/>(decided)"}}
    end
    subgraph lg_edge["Arrow (in the trees every arrow points down, forward in time)"]
        direction LR
        lg_e0a["prerequisite"]
        lg_e0b["depends on it"]
        lg_e1a["old design"]
        lg_e1b["replaced by"]
        lg_e2a["rated node"]
        lg_e2b["qualified by a later finding"]
    end
    lg_g0 ~~~ lg_g1 ~~~ lg_g2 ~~~ lg_shape ~~~ lg_edge
    lg_e0a --> lg_e0b
    lg_e1a -.-> lg_e1b
    lg_e2a -.-> lg_e2b
    linkStyle 6 stroke:#e69500,stroke-width:3px,stroke-dasharray:2 3
    classDef vsucc fill:#2e9e4f,stroke:#1b6b34,color:#fff
    classDef succ fill:#bfe8c8,stroke:#2e9e4f,color:#111
    classDef mixed fill:#ffd37a,stroke:#c98a00,color:#111
    classDef bad fill:#f28b82,stroke:#b3261e,color:#111
    classDef dead fill:#e6e6e6,stroke:#777,stroke-dasharray:5 5,color:#555
    classDef inprog fill:#a8cdf5,stroke:#1f6fc5,color:#111
    classDef next fill:#fff,stroke:#333,color:#111
    classDef blocked fill:#fff,stroke:#b3261e,stroke-dasharray:2 2,color:#111
    classDef parked fill:#f3f3f3,stroke:#999,color:#555
    classDef unrated fill:#fff,stroke:#333,stroke-dasharray:1 3,color:#111
    classDef gate fill:#d9c2f0,stroke:#7b3fb5,color:#111
    classDef decided fill:#ece0f7,stroke:#7b3fb5,stroke-dasharray:2 2,color:#111
    classDef finding fill:#ffe0b2,stroke:#d84315,stroke-width:2px,color:#111
    classDef waiting fill:#e3f2fd,stroke:#1f6fc5,stroke-dasharray:5 5,color:#111
    class lg_s0 vsucc
    class lg_s1 succ
    class lg_s2 mixed
    class lg_s3 bad
    class lg_s4 unrated
    class lg_s5,lg_s6 dead
    class lg_s7 inprog
    class lg_s8 waiting
    class lg_s9,lg_k0,lg_k1,lg_k2,lg_e0a,lg_e0b,lg_e1a,lg_e1b,lg_e2a,lg_e2b next
    class lg_s10 blocked
    class lg_s11 parked
    class lg_s12,lg_k3 finding
    class lg_k4 gate
    class lg_k5 decided
```

Owner calls are the purple shape. A decided owner call is drawn in the overview only, never in the track trees; an open one is listed under *Waiting on the owner*. The prefix on an overview box is its track. A prerequisite implied by a longer chain is not drawn; the node tables keep every edge.
<!-- END GENERATED: legend -->

## Ratings

<!-- BEGIN GENERATED: ratings -->
0 of 4 rated nodes are confirmed by the owner.

Not yet confirmed: `research-lkl-nommu`, `research-unkernel`, `research-flinux-translation`, `research-qemu-emulation`.
<!-- END GENERATED: ratings -->

## Overview

<!-- BEGIN GENERATED: overview -->
```mermaid
flowchart TB
    subgraph bd_0["Nov 2026"]
        research_gvisor_base[/"research · ✔ Pick<br/>gVisor as the base"/]
        ops_name_and_fork{{"ops · ✔ Name it<br/>gvisor-windows"}}
        fs_compat_layer_not_boundary{{"fs · ✔ Compatibility<br/>layer, not security<br/>boundary"}}
    end
    subgraph bd_1["Now (in progress)"]
        podman_inside_gvisor_linux[/"podman · ▶ Podman<br/>inside gVisor on Linux"/]
        platform_tls_register[/"platform · ▶ Set the<br/>TLS register"/]
    end
    subgraph bd_2["Later (blocked or parked)"]
        podman_acceptance_on_windows["podman · ⛔ Run Podman<br/>on Windows"]
    end
    research_gvisor_base ~~~ podman_inside_gvisor_linux
    ops_name_and_fork ~~~ platform_tls_register
    fs_compat_layer_not_boundary ~~~ platform_tls_register
    podman_inside_gvisor_linux ~~~ podman_acceptance_on_windows
    platform_tls_register ~~~ podman_acceptance_on_windows
    research_gvisor_base --> platform_tls_register
    platform_tls_register --> podman_acceptance_on_windows
    classDef vsucc fill:#2e9e4f,stroke:#1b6b34,color:#fff
    classDef succ fill:#bfe8c8,stroke:#2e9e4f,color:#111
    classDef mixed fill:#ffd37a,stroke:#c98a00,color:#111
    classDef bad fill:#f28b82,stroke:#b3261e,color:#111
    classDef dead fill:#e6e6e6,stroke:#777,stroke-dasharray:5 5,color:#555
    classDef inprog fill:#a8cdf5,stroke:#1f6fc5,color:#111
    classDef next fill:#fff,stroke:#333,color:#111
    classDef blocked fill:#fff,stroke:#b3261e,stroke-dasharray:2 2,color:#111
    classDef parked fill:#f3f3f3,stroke:#999,color:#555
    classDef unrated fill:#fff,stroke:#333,stroke-dasharray:1 3,color:#111
    classDef gate fill:#d9c2f0,stroke:#7b3fb5,color:#111
    classDef decided fill:#ece0f7,stroke:#7b3fb5,stroke-dasharray:2 2,color:#111
    classDef finding fill:#ffe0b2,stroke:#d84315,stroke-width:2px,color:#111
    classDef waiting fill:#e3f2fd,stroke:#1f6fc5,stroke-dasharray:5 5,color:#111
    class research_gvisor_base unrated
    class ops_name_and_fork,fs_compat_layer_not_boundary decided
    class podman_inside_gvisor_linux,platform_tls_register inprog
    class podman_acceptance_on_windows blocked
```
<!-- END GENERATED: overview -->

## In plain words

<!-- BEGIN GENERATED: plain-words -->
| title | status | in plain words |
|---|---|---|
| Pick gVisor as the base | ✔ done · unrated | Of all ways to run Linux code on Windows without admin, gVisor's user-space Linux kernel is the only one that already runs containers. |
| Benchmark gVisor against native Linux | ▶ in progress | Measure how much slower process start, small files and CPU work are inside gVisor than natively. |
| Test WHPX on this machine | ▶ in progress | Check whether QEMU can use the Windows hypervisor here without admin, as a fast stopgap. |
| Survey ELF segment alignment | ▶ in progress | Count how often program files place code at addresses Windows cannot map directly (64 KB alignment). |
| Measure syscall trap cost | ▶ in progress | Measure what it costs to catch one Linux system call on Windows and hand it to another process. |
| Test 4 KB section views | ▶ in progress | Check whether Windows can map shared memory at the 4 KB steps Linux programs expect, instead of 64 KB. |
| Set the TLS register | ▶ in progress | Check whether a normal Windows program can set the register Linux programs use for thread-local data; without it native execution fails. |
| Restrict the stub process | ▶ in progress | Check that the process running Linux code can be locked down, so a missed system call does little harm. |
| Recover faults in Go | ▶ in progress | Check that the Go kernel process can survive reading bad guest memory on Windows, as it does on Linux. |
| Compatibility layer, not security boundary | ✔ done · n/a | The sandbox may open the user's files directly with the user's rights; it isolates containers from each other, not from the user. |
| Test persistent image storage | ▶ in progress | Find out where Podman's images can live so they survive a restart of the sandbox, and how fast each option is. |
| Write the Podman acceptance test | ▶ in progress | One test of ten steps defines when Podman counts as working: pull, run, build, mounts, ports, compose, restart, isolation. |
| Podman inside gVisor on Linux | ▶ in progress | Check on Linux whether Podman's engine runs inside gVisor at all; this is the baseline for the Windows port. |
| Run Podman on Windows | ⛔ blocked | The goal: a standard Windows user runs Podman containers with no admin, VM, WSL or Hyper-V. |
| Name it gvisor-windows | ✔ done · n/a | The project is an unofficial fork of gVisor's Go branch called gvisor-windows. |
| Go or no-go after Phase 0 | ⛔ blocked | The owner decides, from the Phase 0 results, whether the port goes ahead. |
<!-- END GENERATED: plain-words -->

## Ready now

<!-- BEGIN GENERATED: ready-now -->
_None._
<!-- END GENERATED: ready-now -->

## Running

<!-- BEGIN GENERATED: running -->
**linux-ci**

- `podman-acceptance-test`: Write the Podman acceptance test
- `podman-inside-gvisor-linux`: Podman inside gVisor on Linux
- `research-gvisor-benchmark`: Benchmark gVisor against native Linux
- `platform-elf-alignment-survey`: Survey ELF segment alignment
- `fs-persistent-image-storage`: Test persistent image storage

**windows-local**

- `platform-trap-cost`: Measure syscall trap cost
- `platform-4k-section-views`: Test 4 KB section views
- `platform-tls-register`: Set the TLS register
- `platform-restricted-stub`: Restrict the stub process
- `sentry-go-fault-recovery`: Recover faults in Go
- `research-whpx-this-machine`: Test WHPX on this machine
<!-- END GENERATED: running -->

## Built, waiting

<!-- BEGIN GENERATED: built-waiting -->
_None._
<!-- END GENERATED: built-waiting -->

## Waiting on the owner

<!-- BEGIN GENERATED: waiting-on-owner -->
- `ops-phase0-go-no-go`: Go or no-go after Phase 0
<!-- END GENERATED: waiting-on-owner -->

## Timeline

<!-- BEGIN GENERATED: timeline -->
[The timeline](roadmap/timeline.md) shows every dated node as a bar from its start to its end.

Generated on 2026-11-26.
<!-- END GENERATED: timeline -->

## Done, by month

<!-- BEGIN GENERATED: done-by-month -->
<details><summary><b>Nov 2026</b>: 5 done, 4 abandoned</summary>

- `fs-compat-layer-not-boundary`: Compatibility layer, not security boundary (✔ done · n/a). The sandbox may open the user's files directly with the user's rights; it isolates containers from each other, not from the user.
- `ops-borrow-code`: Borrow code, check licenses later (✔ done · n/a). Code from other projects may be reused; its origin is recorded so the owner can review licences later.
- `ops-linux-tests-on-actions`: Run Linux tests on GitHub Actions (✔ done · n/a). Everything that needs a Linux host runs on GitHub's hosted runners.
- `ops-name-and-fork`: Name it gvisor-windows (✔ done · n/a). The project is an unofficial fork of gVisor's Go branch called gvisor-windows.
- `research-flinux-translation`: Translate syscalls like flinux (✖ dead end). Mapping each Linux call onto a Windows call cannot provide what containers need, such as namespaces and cgroups.
- `research-gvisor-base`: Pick gVisor as the base (✔ done · unrated). Of all ways to run Linux code on Windows without admin, gVisor's user-space Linux kernel is the only one that already runs containers.
- `research-lkl-nommu`: Revive LKL or nommu UML (✖ dead end). Running the real Linux kernel as a library was rejected: without memory management it cannot fork, and its Windows support is gone.
- `research-qemu-emulation`: QEMU software emulation (✖ dead end). Emulating a whole computer in software works today but is several times slower; it stays only as a stopgap.
- `research-unkernel`: Build on unkernel (✖ dead end). Linux compiled to WebAssembly runs anywhere, but it always emulates the programs, so it cannot reach native speed.

</details>
<!-- END GENERATED: done-by-month -->

## Tracks

<!-- BEGIN GENERATED: tracks -->
Each track has its own page with its tree, node table and per-node details.

- [research - research](roadmap/tracks/research.md): 7 nodes (1 done, 2 in progress, 4 abandoned)
- [platform - platform](roadmap/tracks/platform.md): 5 nodes (5 in progress)
- [sentry - sentry](roadmap/tracks/sentry.md): 1 nodes (1 in progress)
- [fs - files](roadmap/tracks/fs.md): 2 nodes (1 done, 1 in progress)
- [net - network](roadmap/tracks/net.md): 0 nodes
- [podman - podman](roadmap/tracks/podman.md): 3 nodes (2 in progress, 1 blocked)
- [ops - operations](roadmap/tracks/ops.md): 4 nodes (3 done, 1 blocked)
<!-- END GENERATED: tracks -->
