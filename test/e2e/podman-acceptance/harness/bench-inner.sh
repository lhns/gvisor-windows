#!/bin/sh
# Runs inside a container (runc or runsc). Prints "RESULT <name> <seconds>" lines.
# $1 = tarball with many small files, $2 = extra directory on a bind mount (gofer under runsc)
now() { date +%s.%N; }
timeit() { n=$1; shift; s=$(now); "$@" >/dev/null 2>&1; e=$(now); echo "RESULT $n $(echo "$e $s" | awk '{printf "%.2f", $1-$2}')"; }
loop() { i=0; while [ $i -lt 2000 ]; do /bin/true; i=$((i+1)); done; }
timeit forkexec_2000x_true loop
mkdir -p /root/untar && timeit untar_20k_files_rootfs tar -xf "$1" -C /root/untar
mkdir -p /dev/shm/untar && timeit untar_20k_files_tmpfs tar -xf "$1" -C /dev/shm/untar
mkdir -p "$2/untar" && timeit untar_20k_files_bindmount tar -xf "$1" -C "$2/untar"
timeit rm_20k_files_bindmount rm -rf "$2/untar"
cpu1() { head -c 1073741824 /dev/zero | sha256sum; }
timeit cpu_sha256_1GiB cpu1
cpu2() { head -c 536870912 /dev/zero | gzip -6 -c; }
timeit cpu_gzip_512MiB cpu2
