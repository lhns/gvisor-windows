#!/usr/bin/env bash
# Stage E (gate 0a.4): podman pull + small build timings with the store on tmpfs / a host (gofer) dir,
# and whether runsc's --overlay2=root:dir= upper layer survives a sandbox restart.
# Output: $RESULTS_DIR/storage.tsv (variant, op, seconds, rc) and overlay-dir.txt
set -u
R=${RESULTS_DIR:-results}; mkdir -p "$R"
HERE=$(cd "$(dirname "$0")" && pwd)
P=public.ecr.aws/docker/library
OUT=$R/storage.tsv; : >"$OUT"
export SOCK=/tmp/podman-storage.sock NAME=podman-storage
now() { date +%s.%N; }
tm() { # variant op cmd...
  local v=$1 o=$2; shift 2
  local s e rc; s=$(now); timeout 900 "$@" >"$R/storage-$v-$o.log" 2>&1; rc=$?; e=$(now)
  printf '%s\t%s\t%.1f\t%s\n' "$v" "$o" "$(echo "$e $s" | awk '{print $1-$2}')" "$rc" | tee -a "$OUT"
}
b=$(mktemp -d)
printf 'FROM %s/alpine:3.20\nRUN mkdir /d && i=0; while [ $i -lt 3000 ]; do echo $i >/d/f$i; i=$((i+1)); done\nRUN cp -a /d /d2 && rm -rf /d\n' "$P" >"$b/Containerfile"
workload() { # variant sock
  local v=$1 pm="podman --url unix://$2"
  tm "$v" pull_alpine $pm pull -q $P/alpine:3.20
  tm "$v" pull_debian_slim $pm pull -q $P/debian:12-slim
  tm "$v" pull_python_slim $pm pull -q $P/python:3-slim
  tm "$v" build_3000_files $pm build -t st-build "$b"
  tm "$v" run_after $pm run --rm $P/alpine:3.20 true
}
# native reference
SOCK=/tmp/podman-native.sock bash "$HERE/engine-native.sh" start && workload native /tmp/podman-native.sock
SOCK=/tmp/podman-native.sock bash "$HERE/engine-native.sh" stop
sudo podman system reset -f >/dev/null 2>&1

variant() { # label storage driver
  local label=$1; export STORAGE=$2 STORAGE_DRIVER=$3
  sudo rm -rf /srv/podman-storage/* 2>/dev/null
  if bash "$HERE/engine-runsc.sh" start; then workload "$label" "$SOCK"
  else printf '%s\tengine_start\t0\tFAIL\n' "$label" | tee -a "$OUT"; fi
  bash "$HERE/engine-runsc.sh" stop
}
variant runsc-tmpfs-default tmpfs ""
variant runsc-tmpfs-vfs tmpfs vfs
variant runsc-hostdir-vfs hostdir vfs
variant runsc-hostdir-default hostdir ""

# --overlay2=root:dir= persistence across sandbox restart (rootfs upper layer, not podman storage)
{
  echo "## --overlay2=root:dir=/srv/overlay-dir"
  docker rm -f ovl >/dev/null 2>&1
  docker run -d --name ovl --runtime runsc-ovldir $P/alpine:3.20 sh -c 'echo persisted >/in-upper; sleep 600' && sleep 5
  echo "files in host dir while running:"; ls -la /srv/overlay-dir
  docker exec ovl cat /in-upper
  docker restart -t 0 ovl >/dev/null; sleep 5
  echo "after docker restart:"; docker exec ovl cat /in-upper && echo "SURVIVED restart" || echo "LOST on restart"
  docker stop -t 0 ovl >/dev/null; docker start ovl >/dev/null; sleep 5
  echo "after stop/start:"; docker exec ovl cat /in-upper && echo "SURVIVED stop/start" || echo "LOST on stop/start"
  echo "files in host dir at end:"; ls -la /srv/overlay-dir
  docker rm -f ovl >/dev/null
  echo "## --overlay2=root:self (default) for comparison"
  docker run -d --name ovl2 --runtime runsc $P/alpine:3.20 sh -c 'echo persisted >/in-upper; sleep 600' && sleep 5
  docker restart -t 0 ovl2 >/dev/null; sleep 5
  docker exec ovl2 cat /in-upper && echo "SURVIVED restart" || echo "LOST on restart"
  docker rm -f ovl2 >/dev/null
} 2>&1 | tee "$R/overlay-dir.txt"
