# 3e: late timer signals in a guest (sentry-epoll-timeout-overshoot)

Verdict: the 60 ms to 3.8 s late timer signals came from runsc's debug log: the Sentry logs a signal send under the signal mutex, and a write to the log file on a CI runner can block for seconds, so the log is now written from its own goroutine. After the fix no timer signal was later than 3 ms in 6000 samples, a 3.5 s log write stall did not delay any signal, and the epoll_wait check is back to one try.

## Cause

A goroutine dump taken when a timer signal's send was still not done after 50 ms
(windows-runsc run 46082420901, rewrite-fxsave, sandbox boot1: nanosleep woken 2640 ms late):

- the timer goroutine (`SampledTimer.tick` -> `itimerRealListener.NotifyTimer` -> `ThreadGroup.SendSignal`)
  holds the TaskSet read lock and the signal mutex and sits in `WriteFile` on the debug log, writing
  `Notified of signal 14` (`Task.Debugf` in `sendSignalTimerLocked`);
- the timekeeper goroutine waits for the same file inside `VDSOParamPage.Write`, logging a
  `CalibratedClock` line while it rewrites the guest's clock parameters.

runsctest runs every sandbox with `--debug --debug-log` (all of a case's sandboxes append to one file
under `C:\ci\work`). `runsc/winrunsc` wrote that log synchronously, so a blocked write held up the
signal send, and the guest's wait ended only after the write finished. Why a write blocks this long
is unknown: the Windows cache manager throttling writers, or a filter driver. Three of the four
multi-second events were in the second sandbox of a run (boot1).

How it was found (trace on branch `sentry-siglat` at e75e051e6, not merged; see below):

| run | what it showed |
|--|--|
| 46077108221 | Outliers in the stage from the timer firing to the task being reached (44, 58, 121 ms) while the heartbeats of both processes had no gap. One 38 ms outlier was a machine stall: STALL lines in both processes at the same QPC time, and the timer itself ran 20 ms late. |
| 46079666645 | The stage split into fire->sent and sent->out, plus one pinned heartbeat per CPU (HIGHEST priority). `timer_create` one-shot 20 ms took 2275 ms (FAIL). |
| 46081405924 | fire->sent (inside `SendSignal`) took 3813, 1337, 127 and 58 ms. Per-CPU heartbeats at TIME_CRITICAL showed no gap over 13 ms in that leg, so the machine and every CPU were running. |
| 46082420901 | The goroutine dump above. |
| 46083880064, 46084404084 | New case `logstall` before the fix: with every "Notified of signal" log write delayed by 150 ms, 7 timer checks fail, the same checks that failed on runners (nanosleep EINTR, timer_create, ITIMER_REAL, epoll_wait EINTR). |
| 46084995179 | After the fix: `logstall` and `siglat` pass in all legs. A 3498 ms log write in rewrite-fxsave boot1 delayed no signal. |

What it was not: the platform's interrupt. Interrupt latency stayed under 1 ms. Refusals were
"waking" (a thread just answered) and, rarely, "stub", each costing a 50 us retry. Lost wakes in the
reply/request handshakes are ruled out by code reading. Runner CPU stalls are real but small: the
per-CPU TIME_CRITICAL heartbeats saw single-CPU gaps of 20 to 52 ms and machine-wide freezes of
7 to 47 ms, a few per leg run. Those explain the remaining outliers up to about 24 ms (for example a
ppoll 23.8 ms late that ended inside a 41 to 47 ms freeze of every heartbeat), within the checks'
100 ms slack.

## Fix

- `runsc/winrunsc/asynclog_windows.go`: the debug log is written from its own goroutine. Write copies
  the line and returns; it waits only when 8 MB are queued. `closeLog` writes out the queue, also
  when `newInstance` fails after opening the log. Lines still queued when runsc dies in a panic are
  lost; on a clean exit the log is complete (the `strace` and `logstall` cases check this).
- Test hook `GVISOR_TEST_LOG_STALL=<ms>` (off by default) delays each "Notified of signal" log write.
  runsctest case `logstall` (default set) runs the sleep, ptimer, itimer and timeouts groups with
  150 ms. It requires `clocks ok` and at least 20 such lines in the log.
- `test/guests/clocks.c`: the timer-interrupted epoll_wait has one try again. The retry and
  `noretry` are gone. New: `repeat=N`; the group `siglat` (not in the default run), with kinds
  eintr (ITIMER_REAL 30 ms ends a 2 s epoll_wait), spin (the timer fires while the guest spins:
  the platform's interrupt) and timeout (a plain 30 ms epoll_wait); a line with the realtime stamp
  for every wait more than 5 ms late.
- runsctest case `siglat` (dispatch only): 20 sandboxes of the whole clocks guest, then 5 sandboxes of
  `repeat=40 timeouts siglat`. It prints per kind n/min/p50/p90/p99/p99.9/max and counts over 5 and
  20 ms, the outliers, and the heartbeat gaps of runsctest (`pkg/stallwatch`: a goroutine, a locked
  thread and one TIME_CRITICAL thread pinned per CPU) around each traced signal. The default
  `clocks` case runs without any tracing.

## Results

Latency past the requested time, ms (guest CLOCK_MONOTONIC). eintr, spin and timeout: n = 1000 each.
epoll_wait EINTR: 220. Other timer signals (ITIMER_REAL, timer_create, alarm): 100. All waits and
sleeps: 3060.

Before (run 46081405924, trace build):

| leg | eintr p50/p99/max | spin p50/p99/max | timeout p50/p99/max | other timer signals max |
|--|--|--|--|--|
| native | 0.44 / 1.30 / 2.06 | 0.53 / 1.13 / 23.39 | 0.39 / 1.89 / 2.77 | 1.03 |
| rewrite | 0.47 / 1.00 / 2.17 | 0.60 / 1.30 / 23.56 | 0.42 / 1.42 / 13.18 | 0.87 (a CPU-time timer 3828) |
| rewrite-fxsave | 0.48 / 0.91 / 127.76 | 0.52 / 1.12 / 1337.04 | 0.29 / 1.49 / 2.55 | 0.83 |

After (run 46084995179, fix):

| leg | eintr p50/p99/max | spin p50/p99/max | timeout p50/p99/max | epoll_wait EINTR max | other timer signals max | waits max |
|--|--|--|--|--|--|--|
| native | 0.43 / 1.31 / 2.09 | 0.59 / 1.01 / 2.88 | 0.38 / 1.71 / 2.40 | 1.17 | 1.07 | 23.79 (machine freeze) |
| rewrite | 0.44 / 0.83 / 2.09 | 0.52 / 0.90 / 1.29 | 0.28 / 0.75 / 2.62 | 0.80 | 0.83 | 5.10 |
| rewrite-fxsave | 0.45 / 0.78 / 1.95 | 0.53 / 0.90 / 1.24 | 0.30 / 0.84 / 2.53 | 0.97 | 0.75 | 4.79 |

CPU-time timers (ITIMER_VIRTUAL, ITIMER_PROF, CPUTIME) are 6 to 17 ms off in every run: CPU time is
counted in 10 ms ticks (3d), and they are not part of this issue.

Final code: see the runs in the tech-tree node (windows-runsc, windows-sentry-guests).

## The trace (branch only)

Sha e75e051e6 on `sentry-siglat` holds the full trace, left out of the merge so the weekly upstream merge
does not collide with hooks in kernel files: `GVISOR_SIGLAT=<ms>` (`pkg/sentry/kernel/siglat.go`
and hooks in posixtimer, task_acct, task_block, task_run, task_signals; `ktime` tick lateness;
`winstub/siglat_windows.go` with interrupt refusal reasons and latency) prints a SIGLAT line per
late timer signal, with the stages fire, sent, out, deliver, resume and return, the CPU at each, the
platform's account of the interrupted Switch, and QPC times. It dumps every goroutine when a send
takes 50 ms. runsc also times its debug log writes there. To use it, dispatch windows-runsc
`only=siglat` on that sha.

## Open

- Why a `WriteFile` to the debug log blocks for seconds on runners is unexplained. Any other Sentry
  path that does blocking host I/O under a lock would show the same stall. The log was only the one
  that was hit.
- In the trace build, `ktime` tick lines of 200 to 500 ms appeared three times per boot in the ptimer
  group. They matched no late guest measurement (probably timers nobody waits for: SIGEV_NONE, or a
  timer whose signal is blocked) and were not looked into.
- A pending interrupt that meets a thread in its "waking" window is retried every 50 us
  (`time.Sleep`). Harmless at these rates.
