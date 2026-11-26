# Phase 0a: Linux validation results

CI run: https://github.com/lhns/gvisor-windows/actions/runs/45850082760 (workflow `.github/workflows/phase0-linux.yml`, branch `phase0-linux`, ubuntu-latest, x86_64).
Versions: runsc release-20261123.1 (platform systrap), podman 5.7.0 engine (`quay.io/podman/stable`) with crun inside the sandbox, podman 5.7.0 client from the runner's apt.
Raw artifacts (per-step logs, runsc debug logs, probes) are attached to the run.

## How it runs

The sandbox is a docker container with `--runtime runsc` (`scripts/phase0/engine-runsc.sh`). Inside it: rootful `podman system service` on TCP 8888, published by docker and turned into a unix socket on the host by `socat`. The acceptance test (`test/e2e/podman-acceptance/run.sh`, steps in `steps.tsv`) runs on the runner as an ordinary podman client against that socket, so it is the same client-over-socket shape as the Windows target.

### runsc and engine settings that were needed

| Setting | Where | Needed? |
|---|---|---|
| `--platform=systrap` | runsc | used for everything |
| `--in-sandbox-cgroup=v2` | runsc | **required**: without it podman 5 exits with "Cgroups v1 not supported" (gVisor mounts cgroup v1 by default) |
| `--net-raw`, `--allow-packet-socket-write` | runsc | set as in gVisor's Docker-in-gVisor tutorial; not tested without them |
| `--cap-add all --security-opt seccomp=unconfined --user root` | docker run | set (rootful podman needs the caps inside the sandbox) |
| `/var/lib/containers` on a docker `--tmpfs` with `exec` | docker run | **required to be `exec`**: docker's default tmpfs is `noexec` and every container start then fails with "exec container process: Permission denied" |
| `storage.conf`: `driver=overlay`, `mount_program=""`, `additionalimagestores=[]`, plus `CONTAINERS_STORAGE_CONF` env | engine | **required**: the image's default uses `fuse-overlayfs`, whose mounts break inside the sandbox ("lstat .../merged: transport endpoint is not connected"). Kernel overlayfs inside gVisor works on tmpfs and on a gofer host directory (manual `mount -t overlay` probe, `probe-before.txt`) |
| `containers.conf`: `cgroups=disabled`, `log_driver=k8s-file`, `events_logger=file`, `runtime=crun` | engine | used; not tested without |
| `--network=host` for containers | podman | **required**: the default bridge needs netavark, which fails with `netavark: host netlink socket: IO error: Protocol not available (os error 92)` (netlink socket type gVisor doesn't offer) |
| `--overlay2` | runsc | default (root:self) used for the sandbox rootfs |

`docker exec ... uname -a` inside: `Linux 4.19.0-gvisor`. `/dev/fuse` exists but fuse-overlayfs does not work (see above).

## Acceptance test, per step

Variants: `native` = rootful podman on the runner (sanity check of the test). `runsc-*` = podman inside runsc. `bridge` = default container network, `hostnet` = `--network=host` (compose uses `network_mode: host`). Storage: `tmpfs` = podman store on an in-sandbox tmpfs, `hostdir` = store on a host directory mounted into the sandbox (gofer), driver `overlay` (kernel overlayfs) or `vfs`.

| step | native | runsc bridge, tmpfs | runsc hostnet, tmpfs | runsc hostnet, hostdir, overlay | runsc hostnet, hostdir, vfs |
|---|---|---|---|---|---|
| 1 engine reachable | PASS | PASS | PASS | PASS | PASS |
| 2 pull alpine, debian | PASS | PASS | PASS | PASS | PASS |
| 3 run + exit code 42 | PASS | FAIL | PASS | PASS | PASS |
| 4 interactive tty, Ctrl-C | PASS | FAIL | PASS | PASS | PASS |
| 5 build with apt-get | PASS | PASS | PASS | PASS | PASS |
| 6 bind mount, both directions | PASS | FAIL | PASS | PASS | PASS |
| 7 publish port, reach from host | PASS | FAIL | PASS | PASS | PASS |
| 8 compose, two services | PASS | FAIL | PASS | PASS | PASS |
| 9 restart sandbox, images + volume persist | PASS | FAIL | FAIL | PASS | PASS |
| 10 PID + mount namespace isolation | PASS | FAIL | PASS | PASS | PASS |

Why the failures:
- bridge, steps 3, 4, 6, 7, 8, 10: netavark cannot open its netlink socket (`Protocol not available`), so every container with its own network namespace fails to start (exit 126). Step 5 passed in the bridge variant (cause not investigated; the build container evidently got working network). In the bridge variant step 7 (`-p 8080:80`) failed with connection reset (not investigated further); in the host-net variants step 7 uses nginx on 8080 with `--network=host`, so `-p` port publishing never worked in any variant.
- tmpfs, step 9: the store lives in sandbox memory and is gone after the sandbox restarts. Expected, not a bug in the test.
- Step 8 in the host-net variants reaches the other service at `127.0.0.1`, not by service name (no container DNS without a bridge network). Name resolution between services is therefore **not** verified.
- Step 4 runs under `script(1)` (pty on the runner), input and SIGINT reach the container process.

## Benchmarks (seconds, lower is better)

Single run each on a shared GitHub runner; run-to-run noise is large (the previous run measured fork/exec runsc 2.84 s and bindmount untar 1.35 s). "native-host" is the plain runner, "docker-runc" the same debian image under runc, "docker-runsc" the same image under runsc/systrap. Compare runsc against runc, not against the host.

| workload | native-host | docker-runc | docker-runsc | runsc/runc |
|---|---|---|---|---|
| fork+exec 2000x /bin/true | 1.05 | 0.89 | 5.27 | 5.9x (4.6x in the other run) |
| untar 20k small files, container rootfs | 0.67 | 1.37 | 0.75 | 0.5x |
| untar 20k small files, tmpfs (/dev/shm) | 0.69 | 0.50 | 0.51 | 1.0x |
| untar 20k small files, bind mount (gofer) | 0.77 | 0.96 | 2.73 | 2.8x (2.0x) |
| rm -rf 20k files, bind mount | 0.40 | 0.49 | 1.24 | 2.5x (1.6x) |
| sha256 of 1 GiB from /dev/zero | 0.91 | 4.33 | 6.02 | 1.4x |
| gzip -6 of 512 MiB from /dev/zero | 1.37 | 3.32 | 3.81 | 1.1x |

(The container runs of the CPU jobs are slower than the host for runc too, probably the pipe through `head` and a different sha256 build in the image; the runsc/runc ratio is the meaningful number.)

## ELF survey (gate for R2, 64 KiB mapping granularity)

Every ELF file in the exported image rootfs; `scripts/phase0/elf-survey.py`. "offset mismatch" = PT_LOAD with `(p_vaddr - p_offset) % 64K != 0`; "unaligned" = `p_vaddr % 64K != 0`.

| image | ELF files | PT_LOAD | offset mismatch | vaddr unaligned | files with a mismatch | files with an unaligned segment |
|---|---|---|---|---|---|---|
| alpine 3.20 | 17 | 68 | 14 (20.6%) | 50 (73.5%) | 14 (82.4%) | 17 (100%) |
| debian 12 | 694 | 2776 | 546 (19.7%) | 2071 (74.6%) | 546 (78.7%) | 694 (100%) |
| python 3 | 1432 | 5620 | 1053 (18.7%) | 4176 (74.3%) | 1053 (73.5%) | 1405 (98.1%) |
| node lts | 1344 | 5272 | 985 (18.7%) | 3915 (74.3%) | 985 (73.3%) | 1318 (98.1%) |

The `p_align` histograms are in each `elf-*` artifact.

## Image storage (gate 0a.4)

Times in seconds through the podman client; "native" = podman on the runner. Build = 3000 small files created in a `RUN`, then copied in a second `RUN`. All runsc variants use host networking for the build.

| op | native | runsc, tmpfs, overlay | runsc, tmpfs, vfs | runsc, host dir, vfs | runsc, host dir, overlay |
|---|---|---|---|---|---|
| pull alpine | 0.9 | 0.8 | 0.9 | 0.9 | 1.0 |
| pull debian slim | 1.4 | 1.8 | 2.2 | 2.6 | 2.6 |
| pull python slim | 1.7 | 2.7 | 3.8 | 7.2 | 3.7 |
| build 3000 files | 1.7 | 3.2 | failed (exit 125) | 7.8 | 5.2 |
| run after | 0.2 | 0.2 | 0.2 | 0.4 | 0.2 |

Persistence: store on a host directory survives a sandbox restart with both the overlay and the vfs driver (acceptance step 9 PASS: images and a volume with data intact). Store on in-sandbox tmpfs does not.

`--overlay2=root:dir=/srv/overlay-dir` (a docker runtime `runsc-ovldir`): the file written into the container's root filesystem was **lost** after `docker restart` and after `docker stop` + `docker start`. The host directory stays empty while the sandbox runs (the backing file is anonymous) and `--overlay2=root:self` loses it as well. So no `--overlay2` medium persists the upper layer across a sandbox restart. (The sandbox rootfs is recreated on restart; podman's own store has to live on a mounted directory instead.)

## Verdicts

- **0a Podman inside gVisor:** Podman 5.7 with crun runs inside `runsc --platform=systrap` on Linux and passes all 10 acceptance steps when containers use host networking, the store is on a host directory (kernel overlayfs or vfs) and runsc runs with `--in-sandbox-cgroup=v2`; with the default bridge network netavark fails on its netlink socket, so only steps 1, 2 and 5 pass, and name-based service discovery and `-p` port publishing remain unverified.
- **0a Benchmarks:** Against runc on the same image, runsc/systrap was about 5x slower on fork+exec (2000 `/bin/true`: 5.3 s against 0.9 s), 2 to 3 times slower on small-file work on a bind mount, about 1.1x to 1.4x on CPU-bound work, and equal or faster on small-file work inside its own rootfs or tmpfs (single noisy CI run).
- **0a ELF survey:** In alpine, debian, python and node images 18.7% to 20.6% of PT_LOAD segments (73% to 82% of ELF files) have `(p_vaddr - p_offset) % 64 KiB != 0`, and about 74% of segments (98% to 100% of files) start at an address that is not 64 KiB aligned, so a 64 KiB-granularity host cannot map ELF segments 1:1 and the conflict path of R2 is the common case.
- **0a.4 Image storage:** Podman's image store on a gofer-mounted host directory works for pull, build and run and survives a sandbox restart (python:3-slim pull 3.7 s with overlay, 7.2 s with vfs, against 1.7 s natively; a 3000-file build 5.2 s against 1.7 s natively), while the in-sandbox tmpfs is faster but lost on restart and no `--overlay2` medium (`self`, `dir=`) persisted the upper layer across a restart.
