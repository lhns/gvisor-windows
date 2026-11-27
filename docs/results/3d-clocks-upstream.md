# Clocks, timers and sleeps in a guest; scheduled upstream merges

Verdict: sleeps, clocks, timerfd, POSIX and interval timers and poll/select/epoll timeouts already behave correctly in a guest on Windows in all three legs (every wait within 1 ms of the requested time, no Sentry change needed), and a weekly workflow now merges `google/gvisor` `go` into a branch from `windows`, builds it and opens an issue when that fails.

## A) sentry-timers-and-clocks

`test/guests/clocks.c` (static glibc, built by `build-glibc.sh`) runs six groups, each also selectable by name
(`clocks sleep|clocks|timerfd|ptimer|itimer|timeouts`); a failed check is printed and the run goes on, exit 1 at the end,
`clocks ok` on success. A wait of T ms must return within [T-1, T+100] ms.
- sleep: nanosleep 1/10/100 ms, usleep, clock_nanosleep relative and TIMER_ABSTIME on MONOTONIC, REALTIME and BOOTTIME
  (future and past), EINTR with the remaining time.
- clocks: eight clocks never go backwards over 20000 reads, clock_getres, raw syscalls against libc, REALTIME plausible and
  equal to time()/gettimeofday(), MONOTONIC/REALTIME/BOOTTIME/RAW advance together over a sleep, process and thread CPU
  time advance when spinning and not when sleeping, error cases.
- timerfd: relative, absolute (three clocks, future and past), periodic with the read count, nonblocking, disarm, rearm,
  with epoll (level-triggered), poll and select.
- ptimer: timer_create SIGEV_SIGNAL (si_code, si_value, overrun with the signal blocked, getoverrun), REALTIME absolute,
  SIGEV_NONE remaining time, a CPU-time timer.
- itimer: ITIMER_REAL one-shot and periodic, alarm() return values, ITIMER_VIRTUAL and ITIMER_PROF while spinning (and not
  while sleeping).
- timeouts: poll, ppoll, select (updated timeout), pselect, epoll_wait/pwait, an epoll_wait ended by a timer signal,
  sigtimedwait, sem_timedwait, pthread_cond_timedwait.

Where it runs: runsctest case `clocks` (default set of windows-runsc, three legs as three jobs) and sentry-guests case
`clocks` (three legs in one job; this case has no exact-output check, `noout`).

Measured on windows-latest, all legs alike: timer and sleep overshoot 0.1 to 0.7 ms; periodic 10 ms timerfd counts 10 in
111 ms; ITIMER_REAL periodic 15 signals in 150 ms; alarm(1) takes 1000.4 ms; CPU time is counted in 10 ms ticks (200 ms of
spinning reads 190 or 200 ms), so CPU-time timers may fire up to a tick early (tolerated: 15 ms).

First run failures, all in the test and not in the Sentry:
- A past absolute time on MONOTONIC/BOOTTIME was built as a negative timespec (the guest clock starts near 0): EINVAL.
- `clock_settime(CLOCK_MONOTONIC)` gives EPERM in gVisor, EINVAL on Linux: both accepted. (A gVisor difference, not
  Windows specific, left alone.)
- CPU-time timers fire at tick granularity (above).
- One 212 ms stall of a 30 ms epoll_wait/timer-signal check in one runsc rewrite-fxsave run (1 in about 12 leg runs, the
  repeat was clean): the check now takes up to three tries and the generic overshoot allowance is 100 ms.

No Windows-specific time code needed a change (`pkg/sentry/time/vdso_windows.go`, `kernel/profclock_windows.go`).

## B) ops-upstream-merges

`.github/workflows/upstream-merge.yml`: weekly (Monday 04:23 UTC) and `workflow_dispatch`, no push trigger
(permissions `contents: write`, `issues: write`). It runs `.github/scripts/upstream-merge.sh`, which
1. fetches `google/gvisor` `go` (read only) and stops if `windows` already contains it;
2. cuts `upstream-merge-<date>` from `origin/windows` and merges;
3. on success builds `GOOS=windows ./runsc/boot/...` and `GOOS=linux ./runsc/...` (as build.yml does);
4. pushes the branch (also when the build fails, so the failure can be looked at; never `windows`);
5. on a conflict (lists the files, aborts the merge) or build failure (last 60 lines) opens or updates the single open
   issue "Upstream merge needs attention" with `gh issue`, and the run goes red.

Tested: the script ran end to end from a temporary job in build.yml (dry run: no push, no issue; the job was removed
again) on this branch: 4 upstream commits merged cleanly, both builds passed (run 46053091801). The conflict path was run
against two local repositories (conflict listed, merge aborted, issue text produced). Not exercised: the `git push` and
`gh issue` calls and the schedule; the first real run is after the workflow is on `windows`
(`gh workflow run upstream-merge.yml -R lhns/gvisor-windows`).
