#!/usr/bin/env bash
# Sentry-handled page faults per fork+exec under runsc (systrap).
# Counter: the Sentry's "/task/faults" profiling metric (pkg/sentry/kernel/task_run.go: incremented for every
# application fault that is passed to MemoryManager.HandleUserFault), exported with --profiling-metrics.
# /proc/self/stat minflt is hard-coded to 0 in gVisor, so it cannot be used.
# Each workload runs N times in a loop inside one sandbox; the slope of the final counter over N, relative
# to N=0 (same shell, empty loop), is the cost per fork+exec+wait.
set -uxo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
RES=$PWD/results; mkdir -p "$RES"
W=$(mktemp -d); cd "$W"
ARCH=$(uname -m)
URL=https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}
wget -q "$URL/gvisor.tar.bz2" "$URL/gvisor.tar.bz2.sha512" && sha512sum -c gvisor.tar.bz2.sha512
mkdir x && tar -xjf gvisor.tar.bz2 -C x
sudo cp -r x/gvisor-bin /usr/local/bin/ && sudo chmod -R a+rx /usr/local/bin/gvisor-bin
sudo cp "$(find x -type f -name runsc | head -1)" /usr/local/bin/runsc && sudo chmod a+rx /usr/local/bin/runsc
runsc --version | tee "$RES/runsc-version.txt"

(cd "$HERE" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$W/hello" hello.go)
mkrootfs() { # name image
  mkdir -p "$W/rootfs-$1"; docker pull -q "$2" >/dev/null
  cid=$(docker create "$2" true); docker export "$cid" | sudo tar -x -C "$W/rootfs-$1" --exclude='./dev/*' --exclude='./proc/*' --exclude='./sys/*'; docker rm "$cid" >/dev/null
}
mkrootfs debian mirror.gcr.io/library/debian:12
mkrootfs python mirror.gcr.io/library/python:3-slim
sudo cp "$W/hello" "$W/rootfs-debian/hello"; sudo chmod a+rx "$W/rootfs-debian/hello"

run() { # label rootfs-name loop-body N
  local label=$1 rf=$2 body=$3 n=$4 id="f-$1-$4"
  local b="$W/b-$id"; mkdir -p "$b"
  (cd "$b" && runsc spec && jq --arg r "$W/rootfs-$rf" --arg c "i=0; while [ \$i -lt $n ]; do $body; i=\$((i+1)); done" \
     '.linux.namespaces |= map(select(.type!="network")) | .root.path=$r | .root.readonly=false | .process.terminal=false | .process.args=["/bin/sh","-c",$c]' config.json >c.json && mv c.json config.json)
  local log="$RES/$id.csv"
  sudo rm -f "$log"
  sudo runsc --root="$W/rr" --platform=systrap --network=host --ignore-cgroups \
    --profiling-metrics=/task/faults,/task/syscalls --profiling-metrics-log="$log" --profiling-metrics-rate-us=500 \
    run --bundle="$b" "$id" >"$RES/$id.out" 2>&1
  echo "$id exit $?"; sudo chmod a+r "$log" 2>/dev/null
}
for n in 0 40 120; do run true debian /bin/true $n; run gohello debian /hello $n; done
for n in 0 15 45; do run python3 python "python3 -c pass" $n; done
head -c 1500 "$RES"/f-true-40.csv || true
python3 "$HERE/faults-report.py" "$RES" | tee "$RES/faults.tsv"
