#!/bin/sh
# Builds the glibc guests in a Debian container: its glibc is built for baseline
# x86-64, unlike the runner's (static binaries linked against the runner's libc.a
# execute AVX instructions in their start-up code and die on CPUs without AVX, e.g.
# the QEMU development machine). Usage: build-glibc.sh OUTDIR [all]
# Dynamic guests' libraries are copied to OUTDIR/glibclibs with their paths.
set -e
O=$1; G=test/guests
mkdir -p $O
gcc -static -no-pie -O1 -fstack-protector-all -o $O/hello $G/hello.c
gcc -static -no-pie -O1 -o $O/unixsock $G/unixsock.c
gcc -static -no-pie -O1 -o $O/cfr $G/cfr.c
gcc -static -no-pie -O1 -o $O/vdso $G/vdso.c
[ "$2" = all ] || exit 0
gcc -static -no-pie -O1 -fstack-protector-all -o $O/hello_glibc $G/hello.c
gcc -static-pie -O1 -fstack-protector-all -o $O/hello_glibc_pie $G/hello.c
gcc -static -no-pie -O1 -o $O/fork_glibc $G/fork.c
gcc -static -no-pie -O1 -o $O/jit_glibc $G/jit.c
gcc -static -no-pie -O1 -o $O/clonesys $G/clonesys.c
gcc -static -no-pie -O1 -o $O/clonesys_ns $G/clonesys.c
gcc -O1 -fstack-protector-all -o $O/hello_dyn $G/hello.c
gcc -O1 -no-pie -o $O/hello_dyn_nopie $G/hello.c
gcc -O1 -shared -fPIC -o $O/libplug.so $G/plug.c
gcc -O1 -o $O/dlopen_glibc $G/dlopen.c -ldl
gcc -static -no-pie -O1 -o $O/exec $G/exec.c
gcc -static -no-pie -O1 -pthread -o $O/signals $G/signals.c
gcc -static -no-pie -O1 -pthread -o $O/threads $G/threads.c
gcc -O1 -pthread -o $O/threads_dyn $G/threads.c
mkdir -p $O/glibclibs
for b in $O/hello_dyn $O/dlopen_glibc $O/threads_dyn; do
  ldd $b | grep -o '/[^ ]*' | while read l; do cp --parents -L "$l" $O/glibclibs/; done
done
cp --parents -L /lib/x86_64-linux-gnu/libm.so.6 /lib/x86_64-linux-gnu/libgcc_s.so.1 $O/glibclibs/
