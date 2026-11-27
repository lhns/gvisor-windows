# 1o: rewrite-mode correctness

Verdict: the rewrite-mode scanner had real holes (a decoder that lost its place after `vzeroupper`, BMI, EVEX and CET instructions left 30 `%fs` sites of glibc, 11 of a static guest and every canary check behind them unrewritten, and a veneer that could not find a scratch register killed the guest with SIGILL), these are fixed and covered by new tests, but `aptrename` still dies with `free(): invalid size` in rewrite mode only, and the cause is not in the veneers or their patching (it fails the same way with every `%fs` instruction run through the debugger path, native passes also under heavy CPU load).

## Found and fixed

- **Decoder drift.** `tools/windows/fsscan -audit` compares the decoder's instruction boundaries and `%fs` instructions with `objdump -d` (CI: `windows-fsscan`, artifact `audit.txt`). Before: libc 4710 of 4739 `%fs` instructions found, static guest 1014 of 1025. Causes: `vzeroupper`/`vzeroall` (VEX 0F 77) were decoded with a modrm byte and swallowed the first bytes of the next instruction (`sub %fs:0x28,%rax` canary checks after it were never rewritten and would fault at address 0x28 with a zero base); BMI (`andn`, `shrx`, `rorx`...), mask-register, EVEX and CET (`endbr64`, `rdsspq`, `incsspq`, `saveprevssp`, `rstorssp`) instructions the decoder does not know got the wrong length. Now `unknownLen` and `cetLen` (scan.go) give them their length, and every `%fs` instruction of libc, ld.so, libm, busybox, a static glibc guest, libsqlite3 and a Go binary matches objdump (remaining boundary differences: `fwait` fusion, zero padding, `rdpkru`). `TestDecodeBoundaries`.
- **Scratch register.** An instruction with base rsi, index rbp and a register operand 4..7 without REX (also SSE) had no scratch register left (rbx was reserved for `ah..bh` of any instruction): the site became a debugger site that could not run, the guest got SIGILL. Now rbx is reserved only for 8-bit operands, r8-r15 are used when a REX prefix can be added, and rax/rcx/rdx as the last resort when the instruction does not use them implicitly. Found by `TestFSDifferential`.
- Item 1 (Podman engine death `addr=0x28` in sqlite): the fault is `mov 0x28(%rdi),%rax` with rdi=0 inside libsqlite3 (the registers and code bytes are in the engine log), not an unrewritten `%fs` site; no FS TRIPWIRE could have fired. It is wrong data, probably the same corruption as in aptrename.

## Tests added (all in windows-platform)

- `TestFSDifferential`: 3000 random `%fs` instructions of every addressing form (prefix orders, REX, lock, 8/16/32/64 bit, SSE, BMI VEX, base/index/scale/disp8/disp32/absolute, neighbours that fill windows and rip-relative loads) run natively and rewritten, near and far from the veneers; registers, flags, xmm, memory must be identical. 0 differences.
- `TestFSDifferentialInterrupt`: the same as loops, the rewritten run interrupted at random places (spin-timed), resumed from the registers returned.
- `TestFSRealWindows`: the 1140 real windows of glibc, busybox and the static guest (`testdata/fswindows.txt`, from `fsscan -dumpwin`) with random registers and TLS-like data. 0 differences (near; the far run is slow and timed out in one run).
- `TestFSSeam` (the `%fs` counterpart of `TestSeamRedecode`: a guessed instruction swallows an `%fs` instruction, an immediate that looks like one, an `%fs` instruction cut by the piece boundary; both orders, near and far) and `TestFSSeamBinary` (the text of a real Linux program mapped in pieces cut at random pages in random order: every `%fs` and syscall site patched, no byte changed far from a site; run on the static glibc guest in `fs-flakes`).
- Diagnostics: `WINSTUB_FSUD2=1` (all `%fs` sites through the debugger), `WINSTUB_FSLOG=1` (threads moved out of veneers), `runsctest -fsud2 -fslog`, `fsscan -audit|-targets|-dumpwin`.

## Ruled out for aptrename

- Jump table targets inside windows: `fsscan -targets` finds none in libc (1553 windows), 1 in busybox, 0 in the static guest.
- Window patching and seams: fails the same with `WINSTUB_FSUD2=1` (run 45984535622); `TestFSSeamBinary` passes on the guest's text.
- Slow guest / race: native passes under 8 spinning processes (5 min instead of 20 s).
- Veneer semantics: the differential tests above.

## What is known about the failure

The engine (the forked child) dies about 6 s in, when it begins to call opendir/readdir/closedir, in all runs. `WINSTUB_FSLOG` shows three threads moved out of veneers in every run: two at start-up and one in the errno veneer of `0x41e41f` (`neg %eax; mov %eax,%fs:(%rdx)`), restored scratch register and restart entry present. Open: what the rewrite mode changes for a forked child besides the veneers (the `%fs` base only in the TEB, no repair code in the thread page, the reserved veneer area in the guest range).

## Measurements

- Podman `modes=rewrite runs=8` (run 45999368417, on the decoder fix): the engine started in 6 of 8 runs (runs 2 and 8 lost it at the first request, as before); step 5 (apt) failed in every run with `double free or corruption (!prev)`, `Segmentation fault` or an abort in apt-get itself (Debian's dynamic glibc, so not only the static guest); steps 1-4 and 6-10 passed in the six runs with an engine. Target (8 of 8, steps like native) not met.
- fork+exec (windows-profile, run 45980115921): 100 iterations 2.49 s, with `-aspool=2` 1.33 s, `-aspool=4` 1.23 s (runsc already uses 2 spares). The CPU profile is 87% waiting in `debugLoop`; the trace shows about 30 access-violation debug events per exec (lazily mapped text and data pages) and 3 to 4 DLL-load events of the new stub process, each 0.2 to 2 ms: the cost is the number of debug events per exec, not the Sentry's CPU. Not pursued (scope change).
