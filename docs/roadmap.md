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
0 of 54 rated nodes are confirmed by the owner.

Not yet confirmed: `research-lkl-nommu`, `research-unkernel`, `research-flinux-translation`, `research-qemu-emulation`, `workloads-podman-on-gvisor-linux`, `research-gvisor-benchmark`, `platform-elf-alignment-survey`, `fs-persistent-image-storage`, `platform-trap-cost`, `platform-4k-section-views`, `platform-tls-register`, `platform-restricted-stub`, `sentry-go-fault-recovery`, `research-whpx-this-machine`, `runsc-container-on-windows`, `platform-stub-controller`, `platform-page-verification`, `platform-syscall-fast-path`, `platform-winstub`, `platform-busybox-on-windows`, `fs-windows-directfs`, `fs-change-notifications`, `fs-linux-metadata-on-ntfs`, `fs-handles-do-not-block-users`, `net-netstack-windows-link`, `net-port-forwarding`, `runsc-measure-cli`, `runsc-sandbox-lifecycle`, `ops-windows-ci-standard-user`, `ops-syscall-tests-on-windows`, `sentry-fault-recovery`, `sentry-host-stdio-and-tty`, `fs-root-filesystem-source`, `sentry-threads-and-signals`, `research-file-protocol`, `platform-zpoline-hooks`, `sentry-unix-shim`, `platform-exception-stack`, `workloads-strict-bar-linux`, `platform-static-guest-from-memory`, `net-bridge-networking`, `platform-debug-event-cost`, `platform-syscall-site-survey`, `platform-faults-per-exec`, `platform-controller-wakeups`, `platform-fp-state`, `platform-retire-prototype`, `platform-fast-process-start`, `runsc-windows-boot`, `sentry-dynamic-elf-and-exec`, `fs-attribute-cache`, `runsc-windows-lifecycle-commands`, `workloads-end-to-end-speed`, `sentry-profile-small-file-path`.
<!-- END GENERATED: ratings -->

## Overview

<!-- BEGIN GENERATED: overview -->
```mermaid
flowchart TB
    subgraph bd_0["Nov 2026"]
        research_gvisor_base[/"research · ✔ Pick<br/>gVisor as the base"/]
        ops_name_and_fork{{"ops · ✔ Name it<br/>gvisor-windows"}}
        fs_compat_layer_not_boundary{{"fs · ✔ Compatibility<br/>layer, not security<br/>boundary"}}
        workloads_podman_on_gvisor_linux[/"workloads · ✔ Podman<br/>on gVisor on Linux"/]
        platform_tls_register[/"platform · ✔ Set the<br/>TLS register"/]
        ops_phase0_go_no_go{{"ops · ✔ Go or no-go<br/>after Phase 0"}}
        fs_windows_directfs["fs · ✔ Open Windows<br/>files directly"]
        net_netstack_windows_link["net · ✔ Netstack to<br/>Windows sockets"]
        net_bridge_networking["net · ✔ Bridge<br/>networking inside<br/>gVisor"]
        platform_veh_on_guest_stack_unsafe>"platform · ✔<br/>Exceptions overwrite<br/>guest stacks"]
        platform_winstub["platform · ✔ Windows<br/>platform for the<br/>Sentry"]
        platform_static_guest_from_memory["platform · ✔ Run a<br/>static guest from<br/>memory"]
        platform_fast_process_start["platform · ✔ Start<br/>guest processes fast"]
        runsc_container_on_windows["runsc · ✔ Run a<br/>container with runsc"]
        sentry_profile_small_file_path[/"sentry · ✔ Profile the<br/>slow file and exec<br/>path"/]
        platform_busybox_on_windows["platform · ✔ Run<br/>busybox under the<br/>Sentry"]
    end
    subgraph bd_1["Later (blocked or parked)"]
        workloads_podman_on_windows[/"workloads · ⛔ Podman<br/>passes on Windows"/]
    end
    research_gvisor_base ~~~ workloads_podman_on_gvisor_linux
    ops_name_and_fork ~~~ platform_tls_register
    fs_compat_layer_not_boundary ~~~ platform_tls_register
    workloads_podman_on_gvisor_linux ~~~ ops_phase0_go_no_go
    platform_tls_register ~~~ fs_windows_directfs
    platform_tls_register ~~~ net_netstack_windows_link
    ops_phase0_go_no_go ~~~ net_bridge_networking
    fs_windows_directfs ~~~ platform_veh_on_guest_stack_unsafe
    net_netstack_windows_link ~~~ platform_winstub
    net_bridge_networking ~~~ platform_static_guest_from_memory
    platform_veh_on_guest_stack_unsafe ~~~ platform_static_guest_from_memory
    platform_winstub ~~~ platform_static_guest_from_memory
    platform_static_guest_from_memory ~~~ platform_fast_process_start
    platform_static_guest_from_memory ~~~ runsc_container_on_windows
    platform_static_guest_from_memory ~~~ sentry_profile_small_file_path
    platform_fast_process_start ~~~ platform_busybox_on_windows
    runsc_container_on_windows ~~~ platform_busybox_on_windows
    sentry_profile_small_file_path ~~~ platform_busybox_on_windows
    platform_busybox_on_windows ~~~ workloads_podman_on_windows
    research_gvisor_base --> platform_tls_register
    platform_tls_register -.-> platform_veh_on_guest_stack_unsafe
    platform_tls_register --> ops_phase0_go_no_go
    fs_compat_layer_not_boundary --> fs_windows_directfs
    platform_static_guest_from_memory --> platform_fast_process_start
    fs_windows_directfs --> runsc_container_on_windows
    runsc_container_on_windows --> workloads_podman_on_windows
    net_bridge_networking --> workloads_podman_on_windows
    linkStyle 20 stroke:#e69500,stroke-width:3px,stroke-dasharray:2 3
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
    class ops_name_and_fork,fs_compat_layer_not_boundary,ops_phase0_go_no_go decided
    class workloads_podman_on_gvisor_linux,platform_tls_register,sentry_profile_small_file_path mixed
    class workloads_podman_on_windows blocked
    class runsc_container_on_windows,platform_winstub,platform_busybox_on_windows,fs_windows_directfs,net_netstack_windows_link,platform_static_guest_from_memory,net_bridge_networking,platform_veh_on_guest_stack_unsafe succ
    class platform_fast_process_start vsucc
```
<!-- END GENERATED: overview -->

## In plain words

<!-- BEGIN GENERATED: plain-words -->
| title | status | in plain words |
|---|---|---|
| Run a container with runsc | ✔ done · successful | The main goal: gVisor's runtime runs an OCI container on Windows for a standard user, with no admin, VM, WSL or Hyper-V. |
| Set the TLS register | ✔ done · mixed ⚠ | Check whether a normal Windows program can set the register Linux programs use for thread-local data; without it native execution fails. |
| Own vDSO for clock calls | ⛔ blocked | Time and CPU queries are answered inside the stub, so they need no trap at all. |
| Handle 64 KB mapping conflicts | ⏸ parked | When Linux puts two file pieces inside one 64 KB Windows window, the Sentry copies them into one fresh range instead. |
| Windows platform for the Sentry | ✔ done · successful | gVisor's platform interface implemented for Windows: address spaces, context switches and syscall delivery through stub processes. |
| Run busybox under the Sentry | ✔ done · successful | First end-to-end milestone: gVisor's kernel runs an unmodified busybox shell on Windows. |
| Use hardware acceleration when available | ⏸ parked | Where the Windows Hypervisor Platform is enabled, a second platform could run guests in a hardware VM, chosen automatically like runsc's autoplatform. |
| Support CPUs without FSGSBASE | ▶ in progress | On CPUs or VMs without the instruction to set the thread-local register (like the dev VM), rewrite every %fs-prefixed guest instruction into a veneer that reads the base from per-thread storage; chosen automatically by CPUID. |
| Run a static guest from memory | ✔ done · successful | Thinnest end-to-end slice: the Sentry on Windows runs one embedded static program from its own memory filesystem, without host files, network or threads. |
| Exceptions overwrite guest stacks | ✔ fixed | Windows writes about 3 KB of exception data below the faulting thread stack pointer, so in-process handlers can corrupt Linux code; faults must go to the Sentry as debugger instead. |
| Binary translation as a fallback | ⏸ parked | Translate guest code (as the owner's v3compat does with DynamoRIO on Linux) only if debugger faults or unpatchable syscalls prove too slow, or to support CPUs without FSGSBASE. |
| Start guest processes fast | ✔ done · very-successful | A guest process takes 280-560 ms to start through the Sentry, against 18 ms for a bare stub; profile the gap and keep a pool of ready stubs. |
| Rare hang under heavy exec load | ⚠ open | A guest that forks many children hangs now and then: about 1 in 50 gopodman runs, and 9 in 48 fork+exec loops through runsc.exe with the exclusive root cache (2 in 48 with the shared cache); the shell waits in wait4 with no child task. |
| Unsupported platform calls return errnos | ⚠ open | arch_prctl(ARCH_SET_GS) and ptrace preparation return a raw error from the platform instead of an errno, so the Sentry panics with "Unknown syscall 158". |
| Reuse stub processes across exec | ⛔ blocked | Each fork+exec starts two stub processes at about 10 ms each; recycling a stub on release and execve and mapping memory up front would cut process start further. |
| Event waiting on IOCP | ⏸ parked | Waiting for many host events at once (epoll on Linux) moves to Windows completion ports. |
| Timers and clocks on Windows | ⛔ blocked | The Sentry's clocks and timers run on Windows' high-resolution timers. |
| Profile the slow file and exec path | ✔ done · mixed | Creating a file on tmpfs costs about 1 ms and a fork+exec 32 ms on Windows; find where the time goes (syscall round trips, page faults, debug events, Sentry work) and fix the top causes. |
| Sentry dies with netpoll failed | ⚠ open | After a few hundred socket test cases in one kernel the Sentry dies with "fatal error: runtime: netpoll failed" or stalls; about 1,700 syscall-test cases fail only because of this. |
| fchmod on a standard stream fails | ⚠ open | conmon logs "Failed to chmod stderr" and container logs are lost, so the compose check cannot read them. |
| pasta networking inside the sandbox | ⚠ open | buildah RUN uses pasta, which fails: /proc/sys/net/ipv4/ip_local_port_range is missing and switching into the namespace returns EPERM. |
| Compatibility layer, not security boundary | ✔ done · n/a | The sandbox may open the user's files directly with the user's rights; it isolates containers from each other, not from the user. |
| Open Windows files directly | ✔ done · successful | The Sentry opens NTFS files itself for bind mounts, with its own caches, instead of going through a file protocol. |
| Storage that survives restarts | ⛔ blocked | Implement the chosen writable filesystem so data inside the sandbox outlives a restart. |
| Unix sockets on host folders | ⚠ open | Binding a Unix socket on a Windows folder and connecting to it (by path or via /proc/self/fd) is refused, so Podman cannot reach conmon's attach socket. |
| Netstack to Windows sockets | ✔ done · successful | gVisor's own TCP/IP stack sends guest traffic out through ordinary Windows sockets, like a user-space NAT. |
| Bridge networking inside gVisor | ✔ done · successful | Podman's default network creates a bridge and veth pairs over netlink; gVisor lacks part of that, so it fails today. Fix it in gVisor itself, first on Linux, then it carries over to Windows. |
| Podman on gVisor on Linux | ✔ done · mixed ⚠ | Run the acceptance test on stock gVisor on Linux; whatever fails there is a gap in gVisor itself, not in the port. |
| Podman passes on Windows | ⛔ blocked | The bar for enough features: an unmodified Podman engine passes the acceptance test on gVisor on Windows. |
| Serve Podman to Windows clients | ▶ in progress | The test harness runs the Podman engine on runsc and exposes its API to the stock Windows clients. |
| Second attach-socket failure | ⚠ open | Even with container directories on a tmpfs, Podman's connect to conmon's attach socket is refused; a second cause is not isolated. |
| Pick gVisor as the base | ✔ done · unrated | Of all ways to run Linux code on Windows without admin, gVisor's user-space Linux kernel is the only one that already runs containers. |
| Name it gvisor-windows | ✔ done · n/a | The project is an unofficial fork of gVisor's Go branch called gvisor-windows. |
| Go or no-go after Phase 0 | ✔ done · n/a | Go: the owner asked for the full project; ADR 0004 lists the conditions. |
| Merge upstream gVisor regularly | ○ next | Regularly take in upstream gVisor's Go branch so the port does not drift. |
| Portable zip, no installer | ⛔ blocked | Ship runsc for Windows as a zip or per-user installer that needs no admin. |
| Try it on a real work machine | ⛔ blocked | All evidence comes from GitHub hosted Windows images; the owner runs runsc.exe once on a work machine to check FSGSBASE and whether security software tolerates the generated stub being debugged. |
<!-- END GENERATED: plain-words -->

## Ready now

<!-- BEGIN GENERATED: ready-now -->
**linux-ci**

- `ops-upstream-merges`: Merge upstream gVisor regularly

**windows-ci**

- `platform-rare-hang-under-exec-load`: Rare hang under heavy exec load
- `sentry-netpoll-failure`: Sentry dies with netpoll failed
- `fs-unix-sockets-on-host-folders`: Unix sockets on host folders
- `workloads-conmon-attach-second-cause`: Second attach-socket failure
- `sentry-fchmod-std-stream`: fchmod on a standard stream fails
- `sentry-pasta-in-sandbox`: pasta networking inside the sandbox
<!-- END GENERATED: ready-now -->

## Running

<!-- BEGIN GENERATED: running -->
**windows-ci**

- `workloads-podman-harness`: Serve Podman to Windows clients
- `platform-no-fsgsbase-fallback`: Support CPUs without FSGSBASE
<!-- END GENERATED: running -->

## Built, waiting

<!-- BEGIN GENERATED: built-waiting -->
_None._
<!-- END GENERATED: built-waiting -->

## Waiting on the owner

<!-- BEGIN GENERATED: waiting-on-owner -->
- `ops-run-on-a-work-machine`: Try it on a real work machine
<!-- END GENERATED: waiting-on-owner -->

## Timeline

<!-- BEGIN GENERATED: timeline -->
[The timeline](roadmap/timeline.md) shows every dated node as a bar from its start to its end.

Generated on 2026-11-27.
<!-- END GENERATED: timeline -->

## Done, by month

<!-- BEGIN GENERATED: done-by-month -->
<details><summary><b>Nov 2026</b>: 81 done, 7 abandoned</summary>

- `fs-attribute-cache`: Faster stat walks on host folders (✔ done · mixed). A git-status-like walk of 20000 files takes 1.2-2.4 s through the host folder; an attribute cache fed by the change watcher would bring stat near native.
  - Cold stat walk on a fresh mount is still about 4x native, 99-105 us per file (2026-11-26)
  - erofs xattrs and a persistent safe-save index added (2026-11-26)
  - Watched cache made opt-in (cache=watched); overflow drops the whole cache (2026-11-26)
- `fs-change-notifications`: Windows changes reach the guest (✔ done · successful). Edits made on Windows invalidate the Sentry's caches and arrive in the guest as inotify events.
- `fs-compat-layer-not-boundary`: Compatibility layer, not security boundary (✔ done · n/a). The sandbox may open the user's files directly with the user's rights; it isolates containers from each other, not from the user.
- `fs-handles-do-not-block-users`: Files stay editable on Windows (✔ done · successful). The Sentry holds Windows file handles only briefly, so editors and git can still rename and delete files.
- `fs-linux-metadata-on-ntfs`: Linux file metadata on NTFS (✔ done · successful). Owner, mode bits, symlinks and special files survive on NTFS, using a fixed owner and alternate data streams.
- `fs-persistent-image-storage`: Test persistent image storage (✔ done · mixed). Find a writable filesystem inside the sandbox that survives a restart, and how fast each option is.
- `fs-persistent-storage-choice`: Choose the persistent storage (✔ done · n/a). Decided: persistent data lives as real files in a Windows folder, with Linux metadata in an NTFS stream.
- `fs-root-filesystem-source`: Root filesystem from an erofs image (✔ done · successful). The guest's root filesystem comes from an unpacked OCI bundle directory on Windows.
  - Booting from the image fails until the platform maps non-memory-file pages (2026-11-26)
- `fs-rootfs-import`: Bring container images onto NTFS (✔ done · n/a). Decided: the sandbox root is a read-only erofs image with an overlay on top; persistent data such as Podman storage lives in Windows folders.
- `fs-safe-save-keeps-metadata`: Editor safe-saves keep Linux metadata (✔ fixed). An editor's safe-save replaces the file, which drops its metadata stream, so the guest falls back to the mount defaults for mode and owner.
- `fs-standard-user-delete-rights`: Deletes fail for standard users (✔ fixed). On a host folder as a standard Windows user, rename over an existing file, rmdir and unlink fail with permission denied; Podman cannot extract layers or write images.json.
- `fs-windows-directfs`: Open Windows files directly (✔ done · successful). The Sentry opens NTFS files itself for bind mounts, with its own caches, instead of going through a file protocol.
  - Per-directory case sensitivity works without admin on empty folders (2026-11-26)
  - FIFO, socket and block device opens return ENXIO (2026-11-26)
- `net-bridge-networking`: Bridge networking inside gVisor (✔ done · successful). Podman's default network creates a bridge and veth pairs over netlink; gVisor lacks part of that, so it fails today. Fix it in gVisor itself, first on Linux, then it carries over to Windows.
- `net-netstack-windows-link`: Netstack to Windows sockets (✔ done · successful). gVisor's own TCP/IP stack sends guest traffic out through ordinary Windows sockets, like a user-space NAT.
  - Library-tested only: no guest process has used the NAT yet (2026-11-26)
- `net-nftables-thread-safety`: Make nftables safe for concurrent use (✔ fixed). Podman 6's netavark only speaks nftables, and gVisor's nftables package says it is not thread-safe, so --nftables stays off by default.
- `net-port-forwarding`: Publish guest ports on localhost (✔ done · successful). A port opened in the guest becomes reachable on the Windows machine's localhost.
- `ops-borrow-code`: Borrow code, check licenses later (✔ done · n/a). Code from other projects may be reused; its origin is recorded so the owner can review licences later.
- `ops-linux-tests-on-actions`: Run Linux tests on GitHub Actions (✔ done · n/a). Everything that needs a Linux host runs on GitHub's hosted runners.
- `ops-name-and-fork`: Name it gvisor-windows (✔ done · n/a). The project is an unofficial fork of gVisor's Go branch called gvisor-windows.
- `ops-phase0-go-no-go`: Go or no-go after Phase 0 (✔ done · n/a). Go: the owner asked for the full project; ADR 0004 lists the conditions.
- `ops-podman-only-the-bar`: Podman only as the bar (✔ done · n/a). The project is gVisor for Windows hosts; Podman is only the workload that shows enough features work.
- `ops-syscall-test-strategy`: Choose how to run gVisor's tests (✔ done · n/a). The Go branch ships no tests; decide whether gVisor's unit and syscall tests run from a master checkout with Bazel or get ported.
- `ops-windows-ci-standard-user`: Windows CI as a standard user (✔ done · successful). CI on windows-latest creates a standard user and runs every Windows test as that user.
- `platform-4k-section-views`: Test 4 KB section views (✔ done · successful). Check whether Windows can map shared memory at the 4 KB steps Linux programs expect, instead of 64 KB.
- `platform-busybox-on-windows`: Run busybox under the Sentry (✔ done · successful). First end-to-end milestone: gVisor's kernel runs an unmodified busybox shell on Windows.
- `platform-controller-wakeups`: Controller waits without busy polling (✔ done · successful). The prototype controller polls all stubs on one thread and starves guests on small machines; replace it with spin-then-wait hand-offs per stub thread.
  - Remaining 8-9 %fs repairs per Go run follow debug events, which stop all threads (2026-11-26)
- `platform-debug-event-cost`: Measure debugger fault handling (✔ done · successful). Measure what handling a guest fault as debugger costs without touching thread context, how many events a process start makes, and whether many stubs can be debugged at once.
- `platform-elf-alignment-survey`: Survey ELF segment alignment (✔ done · successful). Count how often program files place code at addresses Windows cannot map directly (64 KB alignment).
- `platform-exception-stack`: Check what exceptions do to stacks (✔ done · successful). Windows writes exception data onto the faulting thread stack; check whether that destroys data Linux code keeps there, and what happens on unmapped stacks.
- `platform-fast-process-start`: Start guest processes fast (✔ done · very-successful). A guest process takes 280-560 ms to start through the Sentry, against 18 ms for a bare stub; profile the gap and keep a pool of ready stubs.
  - Merged head: glibc hello 35 ms median, shell pipelines 150-170 ms (2026-11-26)
- `platform-faults-per-exec`: Count page faults per exec (✔ done · successful). Count how many page faults gVisor takes for one fork plus exec on Linux; times the debugger cost this gives the Windows process-start cost.
- `platform-fp-state`: Save and restore FPU and SSE state (✔ done · successful). Guest floating-point and vector registers must survive every switch into the Sentry and back; the platform does not carry them yet.
- `platform-fs-base-repair`: Restore the TLS register after switches (✔ done · unrated). The stub sets the guest thread-local register on every return to the guest; losses from preemption arrive as debug events and the Sentry restores it.
- `platform-map-files-without-sections`: Map files not in the memory file (✔ fixed). The platform can only map pages of the Sentry memory file; erofs image pages (and any other memmap.File) make the guest fault on its first instruction.
- `platform-memory-release`: Give freed guest memory back (✔ fixed). Decommitted guest memory only partly leaves the working set and never reduces commit charge, because pages stay committed in 1 GB sections.
- `platform-page-verification`: Scan code pages for syscalls (✔ done · successful). Before guest code may run, each page is scanned for syscall instructions; patchable sites become jumps into the stub trampoline, the rest are made to fault into the debugger.
  - Views must be mapped RWX and lowered, raising map cost to 7.9 us per page (2026-11-26)
- `platform-restricted-stub`: Restrict the stub process (✔ done · mixed). Check that the process running Linux code can be locked down, so a missed system call does little harm.
  - Low-integrity child dies at startup on the hosted runner, 0xC0000142 (2026-11-26)
- `platform-retire-prototype`: Retire the stub prototype (✔ done · successful). The prototype under tools/windows/stubproto duplicates the platform code; once its test guests run under the Sentry, delete it.
- `platform-split-syscall-unpatched`: Syscalls split across mappings (✔ fixed). A syscall instruction whose two bytes fall into two separately mapped ranges is not found by the scan, so it would reach the Windows kernel.
- `platform-static-guest-from-memory`: Run a static guest from memory (✔ done · successful). Thinnest end-to-end slice: the Sentry on Windows runs one embedded static program from its own memory filesystem, without host files, network or threads.
  - Process start through the Sentry 280-560 ms, fork +260-410 ms (2026-11-26)
- `platform-stub-controller`: Stub and controller prototype (✔ done · successful). The Sentry debugs a tiny Windows stub process that runs a static Linux program; patched syscalls arrive over shared memory and faults as debug events.
  - Patched syscall 0.30 us, ud2 path 60 us, process start 17.7 ms, fork 19.8 ms (2026-11-26)
  - Trap table moved to 0xB0000: Windows maps fixed regions at 0x10000-0x94000 (2026-11-26)
  - One view per 4 KB page: parts of a view cannot be unmapped (2026-11-26)
- `platform-syscall-fast-path`: Patch syscall sites into jumps (✔ done · successful). Syscall sites are patched like gVisor's usertrap into a jump through a table at 0x60000, so syscalls never raise an exception.
- `platform-syscall-site-survey`: Survey patchable syscall sites (✔ done · mixed). Count how many syscall instructions in real images can be patched into jumps, and how many would need the slow debugger path.
- `platform-tls-register`: Set the TLS register (✔ done · mixed). Check whether a normal Windows program can set the register Linux programs use for thread-local data; without it native execution fails.
  - Fails on the QEMU-guest dev machine without FSGSBASE (2026-11-26)
  - Base lost on every deschedule, repaired by a fault handler (2026-11-26)
- `platform-trap-cost`: Measure syscall trap cost (✔ done · successful). Measure what it costs to catch one Linux system call on Windows and hand it to another process.
- `platform-veh-on-guest-stack-unsafe`: Exceptions overwrite guest stacks (✔ fixed). Windows writes about 3 KB of exception data below the faulting thread stack pointer, so in-process handlers can corrupt Linux code; faults must go to the Sentry as debugger instead.
- `platform-winstub`: Windows platform for the Sentry (✔ done · successful). gVisor's platform interface implemented for Windows: address spaces, context switches and syscall delivery through stub processes.
  - Six platform tests pass with the real memory file (2026-11-26)
- `platform-zpoline-hooks`: Hook syscalls like zpoline (✖ dead end). Rewriting each syscall into a call to a trampoline at address 0 cannot work on Windows.
- `research-file-protocol`: Reach host files over a protocol (✖ dead end). Serving Windows files to the sandbox over NFS, 9P or gVisor's lisafs was rejected: the sandbox runs on the same machine and can open them itself.
- `research-flinux-translation`: Translate syscalls like flinux (✖ dead end). Mapping each Linux call onto a Windows call cannot provide what containers need, such as namespaces and cgroups.
- `research-gvisor-base`: Pick gVisor as the base (✔ done · unrated). Of all ways to run Linux code on Windows without admin, gVisor's user-space Linux kernel is the only one that already runs containers.
  - Low addresses unmappable for a standard user, so zpoline cannot work (2026-11-26)
  - Sandbox side measured: 265 packages, 88 using Linux host calls (2026-11-26)
  - gVisor's gofer uses lisafs, not 9P; directfs exists (2026-11-26)
- `research-gvisor-benchmark`: Benchmark gVisor against native Linux (✔ done · successful). Measure how much slower process start, small files and CPU work are inside gVisor than natively.
- `research-lkl-nommu`: Revive LKL or nommu UML (✖ dead end). Running the real Linux kernel as a library was rejected: without memory management it cannot fork, and its Windows support is gone.
- `research-qemu-emulation`: QEMU software emulation (✖ dead end). Emulating a whole computer in software works but is several times slower, so it is no base for the port.
- `research-unkernel`: Build on unkernel (✖ dead end). Linux compiled to WebAssembly runs anywhere, but it always emulates the programs, so it cannot reach native speed.
- `research-whpx-this-machine`: Test WHPX on this machine (✖ dead end). Check whether QEMU can use the Windows hypervisor here without admin, as a fast stopgap.
- `runsc-container-on-windows`: Run a container with runsc (✔ done · successful). The main goal: gVisor's runtime runs an OCI container on Windows for a standard user, with no admin, VM, WSL or Hyper-V.
- `runsc-measure-cli`: Measure runsc's command side (✔ done · successful). Measure how much of runsc's container management (commands, sandbox, gofer, cli) is Linux-only.
- `runsc-windows-boot`: Boot runsc on Windows (✔ done · successful). runsc's real boot path on Windows (spec, mounts, stdio, network) replaces the test harness that builds the kernel by hand.
  - Each runsc run takes 0.4-0.5 s (2026-11-26)
- `runsc-windows-commands`: Choose runsc commands for Windows (✔ done · n/a). Decide which runsc commands the Windows build supports first and how it is driven.
- `sentry-dynamic-elf-and-exec`: Dynamic programs, exec and pipes (✔ done · successful). Run dynamically linked programs with their interpreter and shared libraries, exec a second program, and connect processes with pipes.
- `sentry-fault-recovery`: Port fault recovery to Windows (✔ done · successful). The Sentry's safe copies of guest memory recover from access violations on Windows instead of crashing.
- `sentry-go-fault-recovery`: Recover faults in Go (✔ done · successful). Check that the Go kernel process can survive reading bad guest memory on Windows, as it does on Linux.
- `sentry-host-stdio-and-tty`: Guest terminal and stdio (✔ done · successful). A guest program's input, output and terminal connect to the Windows console (ConPTY), including Ctrl-C.
- `sentry-memory-file-sections`: Guest memory on Windows sections (✔ done · unrated). The Sentry's memory file becomes Windows section objects, one per 1 GB chunk.
- `sentry-threads-and-signals`: Guest threads and signals (✔ done · successful). Multi-threaded guests (such as Go programs) get one stub thread per guest thread, with signals delivered correctly.
  - A new process inside a guest costs 0.9-1.2 s; busybox pipelines take 3.3-3.8 s (2026-11-26)
- `sentry-unix-shim`: Make Linux names resolve on Windows (✔ done · successful). A Windows copy of x/sys/unix defines the Linux constants and types and stubs the calls, so the Sentry compiles with few edits.
- `sentry-windows-build`: Sentry builds for Windows (✔ done · unrated). gVisor's kernel compiles as a Windows program, with host-specific parts stubbed out.
  - GOOS=windows build of runsc/boot succeeds, CI green (2026-11-26)
- `workloads-acceptance-test`: Write the Podman acceptance test (✔ done · unrated). One test of ten steps defines when Podman counts as working: pull, run, build, mounts, ports, compose, restart, isolation.
  - Native Podman baseline passes all 10 steps (2026-11-26)
- `workloads-acceptance-test-windows`: Acceptance test for Windows (✔ done · unrated). The ten acceptance steps written for Windows, driving the stock Podman and docker clients.
- `workloads-bar-host-network`: Does host networking meet the bar (✔ done · n/a). Decided: host networking is only an interim state; -p and compose service names are required.
- `workloads-gvisor-bridge-netlink`: Bridge networking fails in gVisor (✔ fixed). netavark dies on setsockopt(NETLINK_DUMP_STRICT_CHK) (ENOPROTOOPT) and then on RTM_SETLINK with NLM_F_CREATE (EINVAL); nftables (NETLINK_NETFILTER) is only enabled by a test-only flag.
- `workloads-gvisor-cgroupfs-readonly`: Cgroup filesystem mounted read-only (✔ fixed). With Podman managing cgroups, crun cannot write cgroup.subtree_control because gVisor mounts /sys/fs/cgroup read-only; a remount makes it pass.
- `workloads-gvisor-port-publishing`: Port publishing untested in gVisor (✔ fixed). Publishing container ports with -p and service-name lookup in compose never worked or were never exercised inside gVisor.
- `workloads-podman-api-named-pipe`: Podman API on a named pipe (✔ done · unrated). Expose the Podman engine API on a Windows named pipe owned by the user, instead of a loopback TCP port.
- `workloads-podman-on-gvisor-linux`: Podman on gVisor on Linux (✔ done · mixed). Run the acceptance test on stock gVisor on Linux; whatever fails there is a gap in gVisor itself, not in the port.
- `workloads-podman-probe`: Probe Podman inside the sandbox (✔ done · unrated). Right after busybox works, run podman --version and podman info inside the sandbox; their failures are the real gap list.
- `workloads-strict-bar-linux`: Strict bar on stock gVisor (✔ done · mixed). Find out what stock gVisor on Linux needs for -p port publishing and compose service names, across Podman versions and network modes.
- `fs-case-sensitivity-without-admin`: Case-sensitive names without admin (✔ fixed). A standard user on the hosted runner cannot set per-directory case sensitivity (access denied), so Linux images with names differing only in case need another way, without an admin setup step.
- `fs-flaky-negative-cache-test`: Flaky negative-cache test on host folders (✔ fixed). TestVFSNegativeCacheAndQuickChange failed once on windows-fs after passing on the previous head; a timing race in the change watcher or the test.
- `ops-syscall-tests-on-windows`: gVisor syscall tests on Windows (✔ done · mixed). gVisor's own syscall test suite runs against the Windows port to show which Linux calls work.
  - Linux runsc 81.5%, Windows with network 77.3% of 14620 cases (2026-11-27)
- `platform-veneer-address-in-child`: Veneer addresses differ after clone (✔ fixed). A syscall through a relocated veneer returns into the veneer; after clone into a new address space the veneers sit elsewhere, so the child resumes on a zero page and crashes.
- `runsc-sandbox-lifecycle`: Start and stop sandboxes on Windows (✔ done · successful). runsc creates, starts, signals and deletes a sandbox process on Windows that outlives runsc.exe.
- `runsc-windows-lifecycle-commands`: Container lifecycle commands on Windows (✔ done · successful). create, start, state, kill, delete, exec, ps and list need a sandbox process that outlives runsc.exe and a named control pipe; only run and version exist today.
- `sentry-profile-small-file-path`: Profile the slow file and exec path (✔ done · mixed). Creating a file on tmpfs costs about 1 ms and a fork+exec 32 ms on Windows; find where the time goes (syscall round trips, page faults, debug events, Sentry work) and fix the top causes.
  - Fork+exec only 1.7-2.1x faster with a pool of ready address spaces; stub creation is the floor (2026-11-27)
- `workloads-conmon-sync-pipe`: conmon sees no data on its sync pipe (✔ fixed). Every podman run fails with "conmon bytes \"\": found \x00": conmon reads nothing from its sync pipe while creating the first container inside the sandbox on Windows.
  - Cause 1 fixed: FIFO opens on host folders were rejected by policy, now in-Sentry named pipes (2026-11-27)
- `workloads-end-to-end-speed`: Measure end-to-end speed on Windows (✔ done · mixed). Run the Phase 0 workloads (fork+exec loop, untar of small files, a CPU job) through runsc.exe on Windows and compare with runsc and native on Linux.
- `workloads-overlay-userxattr`: Overlay mounts with userxattr rejected (✔ fixed). podman build fails: the Sentry overlay rejects the userxattr option (or the layer setup) when Podman storage sits on a Windows folder.
  - Overlay upperdir must be tmpfs; /var/tmp is now a tmpfs in the engine spec (2026-11-27)

</details>
<!-- END GENERATED: done-by-month -->

## Tracks

<!-- BEGIN GENERATED: tracks -->
Each track has its own page with its tree, node table and per-node details.

- [runsc - runsc](roadmap/tracks/runsc.md): 6 nodes (6 done)
- [platform - platform](roadmap/tracks/platform.md): 34 nodes (25 done, 2 in progress, 1 next, 2 blocked, 3 parked, 1 abandoned)
- [sentry - sentry](roadmap/tracks/sentry.md): 14 nodes (9 done, 3 next, 1 blocked, 1 parked)
- [fs - files](roadmap/tracks/fs.md): 16 nodes (14 done, 1 next, 1 blocked)
- [net - network](roadmap/tracks/net.md): 4 nodes (4 done)
- [workloads - workloads](roadmap/tracks/workloads.md): 16 nodes (13 done, 1 in progress, 1 next, 1 blocked)
- [research - research](roadmap/tracks/research.md): 8 nodes (2 done, 6 abandoned)
- [ops - operations](roadmap/tracks/ops.md): 11 nodes (8 done, 1 next, 2 blocked)
<!-- END GENERATED: tracks -->
