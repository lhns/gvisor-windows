# 1j: CPUs without FSGSBASE and without XSAVE

Verdict: `runsc.exe` now runs on a CPU without FSGSBASE and XSAVE (the QEMU development machine ran busybox from a host folder in rewrite mode), and the three CI legs (native `%fs`, forced rewrite, rewrite plus fxsave) pass the platform tests, the guest suite and the runsc tests, at a measured cost of about 1.4 ns per `%fs` access and no visible change in start-up or throughput; the dev VM's `hello` guest still dies with SIGILL because that binary executes AVX instructions at start-up, which the VM cannot.

## 1. How many `%fs` instructions there are (measured before relying on the design)

`tools/windows/fsscan` runs the platform's own decoder and window planner over the executable segments of ELF files (CI: `windows-fsscan`, run https://github.com/lhns/gvisor-windows/actions/runs/45926040933; binaries are Ubuntu's, not the Podman rootfs image). "Direct": the instruction itself is long enough for the jump. "Window": shorter, but neighbouring instructions that can be copied (including a rip-relative `mov`/`lea`, the GOT load in front of `%fs:(%reg)`) make a window of enough bytes. "None": the debugger runs it (about 100 us each time). "Near": the veneer is within rel32 reach, 5-byte jump (code below 2 GiB). "Far": 7-byte jump through a pointer (shared libraries and PIE, which Linux guests map high).

| binary | %fs | near direct / window / none | far direct / window / none |
|---|---|---|---|
| glibc libc.so.6 | 4709 | 3103 / 1553 / 53 | 3024 / 1555 / 130 |
| glibc ld.so, libm, bash, busybox | 5917 | 5006 / 892 / 19 | 4939 / 916 / 44 |
| musl libc.so | 857 | 857 / 0 / 0 | 857 / 0 / 0 |
| Go (podman, hello) | 3631 | 3630 / 1 / 0 | 3630 / 1 / 0 |
| python3 + libpython | 3748 | 3745 / 2 / 1 | 3744 / 2 / 2 |
| conmon, crun | 511 | 511 / 0 / 0 | 511 / 0 / 0 |
| netavark (Rust) | 2470 | 2150 / 304 / 16 | 777 / 1088 / 605 |
| aardvark-dns (Rust) | 807 | 653 / 130 / 24 | 222 / 424 / 161 |

No instruction was in an unsupported encoding after the fixes below. Weak spot: Rust PIE binaries mapped high, where 24% of the `%fs` sites have no 7-byte window and fall back to the debugger (correct, slow). Not built: islands near high text (a 2-byte `jmp rel8` to padding that holds the 7-byte jump). Go and glibc, the Podman case, are all direct or window.

## 2. Design as built

Mode chosen at platform start from CPUID (no FSGSBASE: rewrite), `WINSTUB_FSMODE=native|rewrite` forces it. Without XSAVE (or `WINSTUB_FPMODE=fxsave`) the stub saves vector state with `fxsave64`/`fxrstor64` (512 bytes; `pkg/cpuid` hides XSAVE/AVX/AMX from the guest in that forced mode, so the Sentry's FP state format follows, `xgetbv` is never executed).

- `pkg/sentry/platform/winstub/scan.go` (builds on every OS): decoder sweep, classes, window planner, veneer builder. A byte-level parser takes the `%fs` instruction apart (legacy prefixes, REX, VEX, opcode, modrm/sib/disp, immediates) and cross-checks the decoder's memory operand; string ops, moffs forms, rip-relative operands, address-size overrides, cmpxchg8b/16b and control-flow instructions are not rewritten (none occur in the binaries above). The decoder zero-extends a disp32 and does not know some VEX/BMI2 instructions glibc uses on `%fs` (`shlx`, `mulx`): the byte parse is authoritative for those.
- Veneer (`fsVeneer`): displaced instructions before; `mov %r1,%gs:save`; `mov %gs:fsbase,%r1`; `lea (%r1,%base),%r1` if the address has a base; the instruction with `[%r1+index*scale+disp]`; `mov %gs:save,%r1`; displaced instructions after; `jmp *0(%rip)` to the end of the window. The scratch register is chosen not to be one the instruction uses (also not rax/rcx/rdx/rsp, vvvv, and the registers behind ah..bh without REX). Nothing is written to the guest stack, flags are untouched. The guest's `%fs` base and the saved register live in the stub thread's TEB (`TlsSlots[1]` and `[2]`, `gs:0x1488`, `gs:0x1490`); the stub's return paths store `PgFS` there instead of `wrfsbase` (the image is patched at start-up by byte pattern, `stubasm.Image(rewrite, fxsave)`, so no second copy of the assembly).
- Patching: the entry is `jmp rel32` (near) or `jmp *abs32` through an 8-byte pointer stored in front of the veneer (far, no trap-table slot used). A shared claim list keeps syscall windows and `%fs` windows from displacing each other's bytes. Veneers are private to the address space in `[0x190000,0x200000)` (448 KiB, about 7000 sites; identical windows are reused, so W^X rescans and re-mapping do not grow it); when the area is full, or no window exists, the instruction becomes `ud2` and the debugger runs it in a veneer built in the thread's own page (`PgDyn`), the cost of a debug event per execution.
- Stops inside a veneer: `fsFix` (called from `parkAs` for faults, interrupts and ud2 stops) finds the veneer by address, moves the instruction pointer back to the `%fs` instruction (restart goes through the existing `restartAt` map into the veneer, entering after the displaced instructions) or, if the instruction completed (at the restore), forward past it, and refills the scratch register from the TEB slot (read with `NtQueryInformationThread` + `ReadProcessMemory`). `repairFS` is not used in this mode: the CPU's base is never the guest's.
- The low stub area moved up by 0x10000: the dev VM's Windows maps pages at `[0xB0000,0xB2000)` that the CI's does not (found by the first run there: "range is not inside the reserved guest range: [0xb0000,0xc0000)").
- `NewAddressSpace` retries a stub that did not come up (twice), and a Sentry panic after a failed fork (`Unknown syscall 56 error: winstub: the stub did not start`, seen once) is gone with it.

Tests (all run in every leg): `TestFSVeneerEncoding`, `TestFSVeneerVEX`, `TestScanFS` (encodings and classes), `TestFSInstructions` (every shape, near and far, a fault inside a veneer with the scratch register intact, restart at the instruction, the debugger-run fallback), `TestFSInterrupt` (400 random interrupts in a loop of `%fs` read-modify-writes; the memory must show exactly the iterations that completed), `TestFSSpeed`, `TestAVX512State`.

## 3. Overhead

Same machine, same job (`windows-platform`, native leg; `TestFSSpeed`, 5M iterations of `mov %fs:0x28,%rax`, three rounds): native 0.32-0.52 ns per iteration (noise around the plain load's 0.3-0.5), rewrite 1.88, 1.95, 1.89 ns: about 1.4 ns per `%fs` access. Guest suite on one machine (`TIMING2`, medians of 9, two rounds, `windows-sentry-guests` native leg): hello_asm 14.1/13.1 ms native vs 12.8/14.3 rewrite, hello_glibc 31.2/29.1 vs 30.5/32.6, fork_glibc 35.6/40.8 vs 36.0/39.0, hello_go 38.7/38.6 vs 38.4/39.4, gopodman 55.2k ops/s vs 55.9k and 47.6k: within noise. Across legs the numbers are not comparable (the three runners had different CPUs: EPYC 9V45, 7763, 9V74). The `bench` of `tools/windows/bench` runs in each leg of `windows-runsc` for the record, on different runners.

## 4. CI

All three workflows have the matrix `native`, `rewrite` (`WINSTUB_FSMODE=rewrite`, plus `WINSTUB_KEEP_AVX512=1`), `rewrite-fxsave` (`WINSTUB_FSMODE=rewrite`, `WINSTUB_FPMODE=fxsave`). The standard-user steps pass the modes to `runsctest` as flags (`-fsmode`, `-fpmode`): giving the launcher an environment resets the user's TEMP. Final run, commit 48f20fde: platform https://github.com/lhns/gvisor-windows/actions/runs/45933146377, guests https://github.com/lhns/gvisor-windows/actions/runs/45933146135, runsc https://github.com/lhns/gvisor-windows/actions/runs/45933146138, fsscan https://github.com/lhns/gvisor-windows/actions/runs/45933146168. All legs green. (`podman-windows` is not part of this change; it runs native on the runner.)

Things CI found on the way, fixed: the decoder's zero-extended disp32 (Go and static glibc TLS use negative displacements: every Go and static guest died); BMI2 `%fs` instructions in glibc (dlopen guest); an instruction reaching past the end of a scanned range (panic in the veneer builder); the standard-user launcher's TEMP.

## 5. Development VM (QEMU, no FSGSBASE, no XSAVE, no AVX)

`runsc.exe --network=none run -bundle ...\trylocal\bundle local-hello` at commit 48f20fde: the platform starts in rewrite + fxsave mode (it used to stop with `the CPU lacks FSGSBASE`), the guest `hello` runs its start-up and then dies with SIGILL (runsc exit 132) at `c5 f9 6e ca` (`vmovd %edx,%xmm1`) at 0x40318e in `init_cpu_features`' neighbourhood of the static glibc binary: the guest executes AVX there, which the VM does not have; this is the guest binary, not the platform. The same command with `/busybox echo "hi from busybox"` (a copy of the bundle with other args, same runsc, same flags) prints `hi from busybox` and exits 0, with 113+93+10 `%fs` sites rewritten in the first text ranges and one `ud2` fallback.

## 6. The hang under heavy exec load (partial)

The dump of the hung fork+exec loop (run 45923754536, `dump-exclusive-12.txt`) shows the exiting child task blocked in `addressSpace.Release` on `<-as.loopDone`: the debugger goroutine sat in an unbounded `WaitForDebugEvent` after `TerminateProcess`, so a lost or never-delivered exit event blocks the task's exit and with it the parent's `wait4`. `debugLoop` now waits at most 100 ms, and ends when the address space is released or the process handle is signaled. Not verified against the hang itself (it is rare and CI did not reproduce it in this change). The capture-without-SuspendThread idea is not done.

## 7. AVX-512 state (not reproduced)

`TestAVX512State` keeps zmm0, zmm16 (the Hi16_ZMM half of the register file) and k1 across a syscall, a fault and an interrupt and passes on a Zen 5 runner (skips on the others). The Podman crash in `ScanSpanPackedAVX512` has fault address 0xfffffffffffffff8, which is exactly a Go `%fs:0xfffffffffffffff8` load with a zero `%fs` base, so it looks like a lost `%fs` base that the repair path missed, not lost vector state; rewrite mode cannot have it. `WINSTUB_KEEP_AVX512=1` (set in the `rewrite` CI legs, gopodman passes with it) skips the workaround in `runsc/boot/loader.go`; the workaround stays by default until native mode is shown to be clean.

## Open

- Rust PIE binaries: 24% of far `%fs` sites use the debugger path (islands would fix it).
- Veneer area: about 7000 sites per address space; python3 with libpython and libc needs about 8500 `%fs` sites and falls back to the debugger for the rest.
- Stale `restartAt` and `fsUD2` entries after unmapping text are not removed (an address reused for other code could be redirected; same exposure as the syscall veneers).
- A jump table entry pointing into the middle of a window is invisible to the scan (same exposure as the syscall windows).
- Guests built for AVX cannot run on the dev VM.
