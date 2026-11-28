# CI: fewer runs, one build per push

Owner of `.github/workflows/*`: the CI owner. **Workers don't edit `on:` blocks; ask the CI owner.**
`build.yml` fails when any workflow has a `push:` trigger without `branches: [windows]`.

## Before (measured 2026-11-27, the last 400 runs, about 2 hours)

400 runs, 337 on worker branches and 63 on `windows`; total job-min (all runs) 3028.5333333333333 runs 395.
Every push on every branch started the unfiltered suites (fs, net, sentry, rootfs: about 13 job-min) and,
for any change under `pkg/`, `runsc/` or `tools/`, platform, runsc, sentry-guests and podman-windows as well.
Job minutes per run, summed over the run's jobs, split by step class (the classes are by step name, so
builds inside a "Tests" step count as tests; queue = job created to job started):

| workflow | runs | job-min/run | queue s (median/max) | setup | go | build | guest/img | tests/other | artifact |
|--|--|--|--|--|--|--|--|--|--|
| podman-windows | 36 | 21.9 | 28/517 | 0.6 | 0.9 | 3.2 | 0.5 | 15.7 | 0.9 |
| windows-runsc | 38 | 11.5 | 28/184 | 0.4 | 0.5 | 2.1 | 1.0 | 7.1 | 0.2 |
| windows-fs | 49 | 7.8 | 7/240 | 0.2 | 0.5 | 0.0 | 0.0 | 7.1 | 0.0 |
| windows-sentry-guests | 36 | 10.4 | 24/303 | 0.6 | 1.0 | 3.0 | 4.1 | 1.3 | 0.2 |
| windows-gcrepro | 12 | 17.7 | 38/220 | 3.4 | 1.0 | 4.0 | 0.0 | 9.2 | 0.1 |
| podman-acceptance-linux | 4 | 52.7 | 99/944 | 1.9 | 0.5 | 0.0 | 0.0 | 48.2 | 0.7 |
| windows-platform | 35 | 4.8 | 16/230 | 0.6 | 1.0 | 1.5 | 0.0 | 1.6 | 0.1 |
| windows-rootfs | 46 | 2.1 | 12/190 | 0.3 | 0.5 | 0.2 | 0.6 | 0.0 | 0.5 |
| windows-stability | 6 | 15.4 | 2/170 | 0.2 | 0.4 | 1.3 | 0.0 | 13.4 | 0.0 |
| windows-net | 45 | 1.7 | 4/190 | 0.2 | 0.5 | 0.4 | 0.0 | 0.6 | 0.0 |
| windows-sentry | 45 | 1.6 | 4/259 | 0.2 | 0.5 | 0.0 | 0.0 | 0.9 | 0.0 |
| fs-flakes | 8 | 9.0 | 37/148 | 0.4 | 0.6 | 2.1 | 0.0 | 5.6 | 0.1 |
| build | 7 | 1.6 | 6/36 | 0.1 | 0.3 | 0.0 | 0.0 | 1.2 | 0.0 |
| windows-fsscan | 15 | 0.6 | 9/97 | 0.1 | 0.1 | 0.0 | 0.0 | 0.3 | 0.0 |
| windows-profile | 1 | 8.2 | 3/6 | 0.5 | 0.6 | 2.0 | 0.1 | 4.6 | 0.3 |
| windows-spikes | 1 | 7.6 | 88/109 | 1.2 | 2.8 | 0.0 | 0.0 | 3.1 | 0.2 |
| tech-tree | 10 | 0.2 | 3/85 | 0.1 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| windows-icb | 1 | 1.0 | 3/3 | 0.2 | 0.3 | 0.0 | 0.0 | 0.5 | 0.0 |

What the numbers say:
- Fan-out dominates: 84% of the runs came from worker branches; the median push to `windows` that touched
  `pkg/` cost about 115 job-min, of which podman-acceptance-linux 50 and podman-windows 22.
- runsc.exe was built 8 times per push (3 legs of runsc, 3 of sentry-guests via sentryguest, podman, stability), the
  test binaries and guests every time, with `cache: false` on every Windows job.
- Queue time is the other cost: median 3-30 s but up to 944 s when worker pushes pile up.
- Wall clock from push to all 8 Windows-side suites green, 6 pushes to `windows`: median 9.4 min.

## What changed

- `ci.yml` (push to `windows` with path filters, or dispatch) builds once and calls the suites:
  `bin` builds runsc.exe, runsctest, sentryguest, ttydrive, sentrysmoke and the compiled test binaries
  (winstub, winhost, gofer, winnat, rootfs, safecopy, pgalloc) with `tools/windows/ci/build-bin.ps1` and uploads
  them as `tmp-win-bin`; `vet` runs beside it; then platform, sentry-guests, runsc, fs, net, sentry, rootfs and
  podman-windows run as reusable workflows (`workflow_call`) that download `tmp-win-bin`.
- Each suite stays dispatchable alone (`workflow_dispatch`); then its `winbin` step builds only what it needs.
- Go cache: composite action `.github/actions/gosetup`; `bin` saves the build and module cache per commit
  (key `go-Windows-<go.sum>-<sha>`, restore prefix `go-Windows-<go.sum>-`), the others only restore it.
  It hits: 146 MB restored in about 10 s, and building every binary then takes 14 s instead of about 90 s.
- Guests and images are cached on a hash of their inputs: runsc guests (`test/guests/**`, `tools/windows/bench/**`),
  sentry guests (`test/guests/**`), rootfs erofs images (`mkrootfs.sh`, `synth-xattrs.sh`), the podman erofs image
  (podman version, harness files). The base images float (debian, alpine, `podman/stable:latest`): bump `v1` in the
  key to rebuild.
- Legs in a loop: windows-platform and windows-sentry-guests run native, rewrite and rewrite-fxsave in one job
  (`tools/windows/ci/leg.ps1`, `Set-Leg`). windows-runsc keeps three jobs (7 min of tests per leg). windows-fs
  split into an admin job and a standard-user job.
- Triggers: every Windows suite fires on push to `windows` only (through `ci.yml`). nftables, fsscan, tech-tree
  also only on `windows`; syscall-survey and syscall-tests have no push trigger. No scheduled test runs (owner, 2026-11-28:
  CI on push is enough); repeat runs are dispatched when needed. Only upstream-merge runs weekly.
- Every workflow has `concurrency: {group: workflow-ref, cancel-in-progress: true}`; a new push cancels the
  previous run of the same workflow on the same ref. Called suites use their own group (`<name>-<ref>-ci`).
- Moves to Linux: none. Every Windows test package is `//go:build windows` (winstub, winhost, winnat, rootfs), and
  the scanner (fsscan) already runs on Linux.
- Fixed: podman-windows "Steps that must pass" matched with an array `-notmatch` and was always red.
  The vets of winstub, winrunsc and sentryguest are informational as before (known unsafe.Pointer reports);
  winnat vet gates.

- `release` job in `ci.yml`: artifact `runsc-windows-amd64-<shortsha>` on every push to `windows` with `runsc.exe`, a hello/busybox
  OCI bundle, README, NOTICE and LICENSE; try it with `runsc.exe --network=none run -bundle bundle hello`
  (`tools/windows/ci/make-release.ps1`).
- podman-windows "Steps that must pass": all 10 steps, except step 5 outside native mode (it still fails in default and rewrite mode).

## Artifacts

A successful `ci` run has one download and nothing else that outlives a day:

| artifact | when | kept |
|--|--|--|
| `runsc-windows-amd64-<shortsha>` | every ci run: a zip with `runsc.exe`, the hello bundle, README, NOTICE, LICENSE (tested as a standard user by `windows-release`) | 14 days |
| `tmp-win-bin` | `bin` to the suites (`C:\ciin`) | 1 day |
| `release-assets` | `release.yml` only: what the release is created from | 7 days |
| `tmp-runsc-zip`, `tmp-runsc-linux` | `release.yml` hand-offs to its publish job (also `tmp-win-bin`) | 1 day |
| `logs-<workflow>[-<leg>]` | only when the job failed (`logs-windows-runsc-native`, `logs-windows-platform`, `logs-windows-sentry-guests`, `logs-podman-windows-<mode>-<run>`, `logs-fs-flakes-<mode>`, `logs-windows-stability-<mode>`) | default (90 days) |
| `results-windows-profile-<job>` | `windows-profile`, whose point is its measurements: always uploaded | default |

Rules for a workflow change: a hand-off between jobs is named `tmp-*` with `retention-days: 1`; guests and images
that have a hash-keyed cache (runsc guests, sentry guests, rootfs erofs images, the Podman erofs image) are not
uploaded at all: the Linux job saves the cache with `enableCrossOsArchive` and the Windows job restores it with
`actions/cache/restore` and `fail-on-cache-miss` (the key is spelled in both places). Logs are uploaded `if: failure()`.
On success the numbers are in the job summary (`tools/windows/ci/summary.ps1`: step outcomes of runsc, pass counts
per leg of platform and sentry-guests, timings, podman-windows steps, stability and fs-flakes repetitions), so
nobody needs to download anything. Run a failed leg's logs with `gh run download <run> -n logs-windows-runsc-native`.

## How workers run suites now

Pushes to a worker branch start nothing. Run what you need:

```
gh workflow run windows-platform      --ref <branch> -R lhns/gvisor-windows -f only='TestFS' -f legs=native
gh workflow run windows-runsc         --ref <branch> -R lhns/gvisor-windows -f only=erofs,hostfs -f legs=rewrite
gh workflow run windows-sentry-guests --ref <branch> -R lhns/gvisor-windows -f only='fork|sig_' -f legs=native
gh workflow run windows-fs            --ref <branch> -R lhns/gvisor-windows
gh workflow run windows-net | windows-sentry | windows-rootfs | build ...
gh workflow run podman-windows        --ref <branch> -R lhns/gvisor-windows -f only=4 -f modes=native -f runs=1
```
`only` and `legs` are optional (legs: `native`, `rewrite`, `rewrite-fxsave`, comma separated). `ci.yml` itself is
dispatchable once it is on `windows`. A second dispatch of the same workflow on the same ref cancels the first.

## After

Two `ci.yml` runs on `ci-fast` (push), all suites in them green except the first run's podman-windows
(the must-pass bug fixed since), and every changed workflow dispatched alone on `ci-fast` and green:
windows-platform, windows-sentry-guests, windows-runsc, windows-fs, windows-net, windows-sentry, windows-rootfs,
build, podman-windows (runs=1; step 5 fails as known, the run completes and passes).

| | before (push to `windows`) | after (`ci.yml`) |
|--|--|--|
| job-min per push, same suites (platform, runsc, sentry-guests, fs, net, sentry, rootfs, podman-windows) | about 62 | 39.8 (run 45977125027) |
| jobs per push | 3 + 3 + 3 + 1 + 1 + 1 + 1 + 16 + podman | 1 bin + 1 vet + 1 + 3 + 1 + 2 + 1 + 1 + 2 + podman 3 |
| wall, push to all suites green | median 9.4 min (6 pushes) | 9.1 and 9.4 min (2 runs, cache cold then warm) |
| runs started by a worker-branch push | 4 to 11 | 0 |
| builds of runsc.exe per push | 8 | 1 (14 s with a warm Go cache) |
| guests and images | rebuilt every run | cache hit: 10 to 30 s job |

Reading it:
- The saving is in total work and in not starting anything on worker branches (about 3000 job-min in the 2 hours
  measured, 84% of it from worker branches), which is what fills the runner queue (up to 944 s of queueing before).
- The wall clock of one push did not move much: it is bounded by the longest suite (runsc about 5 min, podman-windows
  about 6 min, fs 4 min) plus `bin` (1 to 2 min, then the suites start); the runs above also queued 25 to 110 s per
  job because of the worker load and my own dispatches. Removing the 100 to 160 s "Phase 0 workloads" step from the
  runsc legs (informational, `continue-on-error`) would cut the critical path further; not done, it is test content.
- podman-acceptance-linux (16 Linux jobs, 4.7 min on idle runners, 16 to 18 min when they queue) is untouched
  apart from `concurrency`: it is the slowest required suite and is bound by runner availability. Merging its small
  matrix jobs into fewer ones is the next lever if that wait matters.
