#!/bin/sh
# The Phase 0 workloads (test/e2e/podman-acceptance/harness/bench-inner.sh) for a busybox guest:
# the same work, timed with /proc/uptime. Prints "RESULT <name> <seconds>".
# $1 tarball of 20k small files, $2 a directory on a bind mount,
# $3 a directory on the root filesystem, $4 a directory on tmpfs.
now() { cut -d' ' -f1 /proc/uptime; }
timeit() { n=$1; shift; s=$(now); "$@" >/dev/null 2>&1; e=$(now); echo "RESULT $n $(echo "$e $s" | awk '{printf "%.2f", $1-$2}')"; }
loop() { i=0; while [ $i -lt 2000 ]; do /bin/true; i=$((i+1)); done; }
R=${3:-/root/untar}; T=${4:-/tmp/untar}
timeit forkexec_2000x_true loop
mkdir -p "$R" && timeit untar_20k_files_rootfs tar -xf "$1" -C "$R"
mkdir -p "$T" && timeit untar_20k_files_tmpfs tar -xf "$1" -C "$T"
mkdir -p "$2/untar" && timeit untar_20k_files_bindmount tar -xf "$1" -C "$2/untar"
timeit rm_20k_files_bindmount rm -rf "$2/untar"
cpu1() { head -c 1073741824 /dev/zero | sha256sum; }
timeit cpu_sha256_1GiB cpu1
cpu2() { head -c 536870912 /dev/zero | gzip -6 -c; }
timeit cpu_gzip_512MiB cpu2
