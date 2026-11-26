#!/usr/bin/env bash
# Stage C: same workload native (host), in runc and in runsc. Output: $RESULTS_DIR/bench.tsv
set -u
R=${RESULTS_DIR:-results}; mkdir -p "$R"
IMG=public.ecr.aws/docker/library/debian:12-slim
W=$(mktemp -d); HERE=$(cd "$(dirname "$0")" && pwd)
# node_modules-like tree: 20k files of 0.5-8 KiB in ~2000 dirs
python3 - "$W" <<'PY'
import os, sys, random
random.seed(1); root = os.path.join(sys.argv[1], "tree")
for d in range(2000):
    p = os.path.join(root, "pkg%d" % d, "lib"); os.makedirs(p)
    for f in range(10):
        with open(os.path.join(p, "f%d.js" % f), "wb") as fh: fh.write(os.urandom(random.randint(512, 8192)))
PY
tar -cf "$W/small.tar" -C "$W/tree" . && ls -l "$W/small.tar"
mkdir -p "$W/bind"
run() { # label cmd...
  local label=$1; shift
  "$@" | grep '^RESULT' | awk -v l="$label" '{print l"\t"$2"\t"$3}' >>"$R/bench.tsv"
}
: >"$R/bench.tsv"
# host: inner script as-is (rootfs dir = /tmp)
cp "$W/small.tar" "$W/small-host.tar"
run native-host bash -c "sed 's#/root/untar#$W/rootfs#; s#/dev/shm/untar#$W/shm#' $HERE/bench-inner.sh >$W/inner-host.sh; sh $W/inner-host.sh $W/small.tar $W/bind"
for rt in runc runsc; do
  rm -rf "$W/bind"/*
  run "docker-$rt" docker run --rm --runtime "$rt" -v "$W/small.tar:/small.tar:ro" -v "$HERE/bench-inner.sh:/inner.sh:ro" -v "$W/bind:/bind" "$IMG" sh /inner.sh /small.tar /bind
done
cat "$R/bench.tsv"
