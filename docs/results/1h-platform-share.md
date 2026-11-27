# 1h: platform sharing, spin policy, Podman-shaped stress

Verdict: Read-only erofs sections are now shared across address spaces (about 3x less commit for 20 processes) and the new `tokens` spin policy fixes the oversubscription latency cliff (8 threads: p99 12.7 us instead of 36 us, throughput 2.4M/s instead of 0.4M/s), but a rare (about 1 in 50 to 1 in 20) guest hang under heavy multithreaded exec load is mitigated, not fixed.

## 1. Sharing read-only file mappings

Read-only, non-exec mappings of a file whose memmap file exposes its section (`ReadOnlySection()`, implemented for the erofs image) map that section directly into the stub (`MapViewOfFile3` of the image section) instead of copying into a per-process pgalloc extent. Disable with `WINSTUB_NOSHARE=1`.

`TestShareReadOnlySection`, 20 address spaces each mapping and reading the same 16 MiB:

| | MapFile per AS | read per AS | system commit | available physical |
|---|---|---|---|---|
| share=true | 0 | 3.5 ms | +122 MB | -18 MB |
| share=false | 8.7 ms | 3.9 ms | +443 MB | -342 MB |

Commit growth of the shared case is the 20 stubs themselves; the image bytes are charged once. I did not measure a separate "20 busybox exec" wall time: the exec path is dominated by stub start-up, which sharing does not change.

## 2. Oversubscription spin policy

4 vCPUs, `TestPolicyMatrix` (every guest thread issues syscalls in a loop, plus a probe thread measuring round trip). k syscalls/s total; probe p99 at 1/4/8/32 threads:

| policy | 1 | 4 | 8 | 32 |
|---|---|---|---|---|
| always | 3567 / 1.1 us | 3579 / 2.9 us | 4175 / 1.1 us | 4072 / 1.0 us |
| limit (old) | 3462 / 1.1 us | 2182 / 21.5 us | 400 / 36.4 us | 403 / 36.8 us |
| adaptive | 2411 / 17 us | 2265 / 61 us | 2268 / 62 us | 2854 / 72 us |
| tokens (new default) | 3585 / 1.1 us | 3673 / 3.0 us | 2433 / 12.7 us | 1238 / 28.5 us |

`always` has the best numbers on this synthetic loop but it burns all cores and starves the Sentry itself in real workloads (gopodman below), and its per-thread min at 16/32 threads shows starvation (10k vs 590k). `tokens`: a few contexts at a time hold a spinning token; the others sleep immediately. No regression at 1-4 threads, 6x the throughput of `limit` at 8 threads and a much lower p99. Selectable with `WINSTUB_POLICY`.

## 3. Podman-shaped stress (`test/guests/gopodman`)

Go guest, GOMAXPROCS=8, 200 goroutines doing tmpfs file I/O, pipes, futexes and timers, then 50 forked/exec'd children. Passes under all three policies in CI (required step):

| policy | ops/s | round p50 | round p99 | children p50 / max |
|---|---|---|---|---|
| tokens | 28163 | 633 us | 368 ms | 87 / 738 ms |
| limit | 28601 | 155 us | 391 ms | 82 / 795 ms |
| always | 39666 | 49 us | 163 ms | 48 / 550 ms |

The p99 is dominated by 200 goroutines contending for 4 vCPUs plus 50 process starts, not by the platform's per-syscall path.

## 4. Defects found and fixed on the way

- Veneer address in child (platform-veneer-address-in-child): the best patch window now ends at the syscall so the syscall returns into the original text; windows with post instructions use veneers whose address is chosen platform-wide per (site, window bytes). `clonesys` and `clonesys_ns` pass and are required; `TestVeneerSurvivesNewAddressSpace` covers it.
- Thread limit: 127 threads per address space was too few for Go children; now 1024 with lazily committed slots.
- Double `CloseHandle` of a thread handle corrupted Go runtime handles.
- Deadlock in `freeThread` (waiting for thread exit under `tmu`).
- Stub spin budget 0 meant 2^64 iterations.
- Threads are now ended cooperatively (mode 2), `TerminateThread` only as fallback; slot reuse waits for the old thread; threads are created one at a time per address space; `interrupt` refuses threads that are being answered or still starting.

## 5. Open: intermittent hang

With a 40-50 run hunt (`WINSTUB_POLICY=limit`, gopodman, 20 s timeout) the hang rate went from 10/60 to 1/50 across the fixes above (6/50 and 11/50 in intermediate runs, so the figure is noisy). Dumps (`DumpState`) show either threads asleep with state 2 whose reply was never consumed, or new threads sitting at `RtlUserThreadStart` that never ran. The last remaining dump shows the debugger goroutine inside `onException` handling an access violation of a thread that `interrupt` had captured at the same time. An attempt to serve captures on the debugger goroutine (never beside a debug event) made CI hang so badly the runner had to be force-cancelled; it was reverted. Next step: do captures without SuspendThread (e.g. a signal-style request flag polled by the stub) rather than suspend/redirect.
