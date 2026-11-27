# 1p: rewrite-mode heap corruption

Verdict: nothing wrote to the wrong place: in rewrite mode `Unmap` refused every range that reached the %fs veneer area, so the stub kept pages the memory manager had already released, and a heap that shrank and grew again kept its old pages, which the Sentry no longer wrote to; with `Unmap` leaving only the stub's own views in place, aptrename passes in rewrite mode (fs-flakes 3 of 3 plus 2 of 2 under load, the development VM 5 of 5 without a heap failure, though 2 of them fail on a different check), and apt passes in 16 of 16 Podman runs.

## Cause

- The memory manager widens an invalidation to the whole gap between its pmas (`mm/pma.go`, `invalidateLocked`: "extrapolate to the end of any previous region"). When nothing is mapped above, the gap runs to `layout.MaxAddr`. So a brk shrink at `0x4ec000` arrives as `Unmap(0x4ec000, 0x6fadb6d14000)`.
- In rewrite mode the 32 MiB %fs veneer area at `0x4000_0000` is a sealed stub view inside the guest range (ADR 0020). As soon as a range contains a sealed view, `removeRange` returns `errSealed` before it unmaps anything, and `Unmap` wrote that only to the debug log (read off the code; the logging below showed the effect). Every such invalidation was dropped. Native mode has no sealed view in the guest range, which is why it passed.
- What follows: malloc trims the brk heap (`brk(0x4f4000)` down to `0x4d4000`), but the stub keeps the view `0x4d3000-0x4f4000` on the old memory-file pages. When the heap grows again, the memory manager hands out new pages, but the guest never faults on them because the old view is still there. From then on the guest and the Sentry see different pages at the same addresses: system calls write into the new ones (getdents, read), and the guest reads the old ones, where its malloc metadata also lives.
  - Debian's static glibc spins forever in `readdir` over a zeroed buffer after `getdents64` returned 48 bytes.
  - Ubuntu's aborts with `free(): invalid size`. At the abort, a chunk header recorded as `0x8041` at opendir reads 0.
  - apt and libsqlite3 in Podman hit double frees and NULL pointers.

## How it was found

The probe guests, their build and the probe runner are in commit `ea8724c30` (`test/guests/phc`, run through a borrowed `windows-glibc-diag.yml`, restored since). The view logging was a local build only.

1. **Guests for the development VM.** The VM has no FSGSBASE, XSAVE or AVX, and it cannot run the CI-built guests: Ubuntu's static glibc executes `vmovd` during start-up (`SIGILL` at `0x40446e`). Guests built on Debian bookworm do run there. With them, aptrename does not abort but hangs in rewrite mode, on the VM and in CI (run 46007794272: native 16 of 16 probe cases pass, rewrite hangs for 25 minutes until cancelled).
2. **Probes on every DIR.** Each DIR's pointer and chunk header were recorded at opendir and checked around readdir and closedir.
   - Debian guests: neither ever changed, which rules out a writer to the header and a corrupted pointer.
   - Ubuntu guests (run 46012359179): the header of the 8th DIR (`0x4f48a0`) reads 0 at the abort. That is a page swapped under the address.
3. **Where the guest stopped.** `--strace` showed the engine's last system call: `getdents64(.../partial, 0x4db850, 0x8000) = 48`. The goroutine and stub dump showed the thread spinning at `0x439a97`, which is the `d_reclen` loop of `readdir`. The guest was reading zeros where the Sentry had just written two entries.
4. **The dropped unmaps.** I logged every `MapFile`, `Unmap` and view change below 16 MiB. After the brk shrinks, the platform received `Unmap(0x4ec000, 0x6fadb6d14000)` and others like it, and removed no view. The view `0x4d3000-0x4f4000`, mapped at the first heap growth, stayed for the rest of the run.
5. **The rollbacks are not the cause.** With the stub pid in the FSFIX lines, the rollbacks reported in 1o are harmless:
   - the two at start-up are the parent's errno read in `__libc_fork` and the child's `%fs:0x10` after `clone`;
   - the one in the errno veneer (`neg %eax; mov %eax,%fs:(%rdx)`) is in the parent's `access()`, not in the process that dies.

Not needed in the end: the hardware watchpoints (implemented in `ea8724c30`, not used once the probes had answered), and the COW and patch-after-fork checks.

## Fix

- **`Unmap`.** It now splits the range around the stub's sealed views and removes the rest (`aspace_windows.go`). A failing `UnmapViewOfFile2`, ignored before, is logged.
  - `TestUnmapAcrossStubViews` (windows-platform, all legs): an unmap from a mapped page to `0x6000_0000_0000` must remove the guest pages below and above the veneer area and keep the area itself.
  - It has not been run against the old code in CI. On the VM, the same Debian bundle hangs at round 2 before the fix and passes 200 rounds after it.
- **Stale TEB address (found on the way).** A thread slot reused by a new Windows thread kept the TEB address of the old one (`t.teb` is read once). Moving a thread out of a veneer would then restore its scratch register from another thread's TEB. `createThread` now resets it, and `t.dyn` too. This is not the cause here (one thread per process), but Podman's Go programs do reuse slots.
- **`WINSTUB_FSUD2`.** Nothing read `WINSTUB_FSUD2=1`, so the `rewrite-ud2` legs of fs-flakes ran the normal veneers, and 1o's "fails the same with every %fs instruction through the debugger" never tested that. The flag now sends every %fs site to the debugger path.
- **`TestFSSeamBinary`.** The test failed on this commit for an unrelated reason. The Go guest's ELF header page holds its build ID, which changes with every commit, and here it contains `64 6f` (`outsd` with %fs). The platform leaves that alone by design, but the test counted it as an unpatched site. The test now skips %fs instructions the veneers cannot express.

## Results

Product code at `bce41cf19`; `f582c1093` changes only the tests and the windows-platform test list.

| check | result |
|--|--|
| fs-flakes 46016193396, rewrite | aptrename 0 of 3 failed, 0 of 2 under CPU load; mountkeep, ovlsub, devnull, fsstress pass |
| fs-flakes 46016193396, rewrite-ud2 (now really through the debugger) | still running when this was written (in_progress ) |
| fs-flakes 46016193396, native | passes |
| development VM, rewrite + fxsave, Debian guest, 200 rounds, final build | 3 of 5 pass (before the fix: hangs at round 2). The other 2 fail `layers-empty` (rounds 3348, and 445 and 735): a read of `layers.json` during its replacement returned nothing. That is the host-folder replace check on a slow machine, not the heap. No abort and no hang in any of them. |
| windows-platform 46018309395 (`f582c1093`) | green in all legs; `TestUnmapAcrossStubViews` passes in native, rewrite, rewrite-fxsave |
| windows-sentry-guests 46016206784, windows-runsc 46016217615 | green in all legs |
| podman-windows 46016231319, `modes=rewrite runs=8` | step 5 (apt) passes 8 of 8 (1o: 0 of 8); all 10 steps in 7 of 8; run 4: the engine died at step 8 (see below) |
| podman-windows 46020575644 (`f582c1093`), `modes=rewrite runs=8` | all 10 steps in 8 of 8 |

No run logged `winstub: Unmap(` or `unmapping the view`.

## Open: an `asyncPreempt` return into a patched window

In run 4 of 46016231319 (not again in the 8 runs of 46020575644) the Podman engine (Go) died with `unexpected fault address 0x4890908f` at `pc=0x1dd367d4` in `unix.Lgetxattr`, 1 ms after a SIGURG was delivered to that thread.

- **The code there.** The original bytes are `49 c7 c6 f8 ff ff ff` (`mov $-8,%r14`), then `64 4d 8b 36` (`mov %fs:(%r14),%r14`, Go reloading `g` after a call), then at `0x1dd367d4` `48 8b 44 24 48`. So the faulting address is the instruction right after a rewritten %fs instruction, inside its window.
- **The fault.** `0x4890908f` is `rax` (-1, the syscall's r1) plus `0x48909090`. The CPU was decoding the window's nop filler as a displacement.
- **Likely cause.** Go's SIGURG handler injects a call to `asyncPreempt` with the interrupted pc as return address. The platform had moved the thread out of the veneer to `w0 + afterO`, which is an address inside the window. The `ret` of `asyncPreempt` lands there directly, past `restartTarget`.
- **Possible fix.** Never hand the Sentry an address inside a window. Run the thread to the end of the window (or back to its start) before parking it, or choose windows in which the %fs instruction is last and nothing before it is displaced. This is a design choice for the rewrite mode, so I have not made it here.
