d=$(dirname $(dirname $(dirname $(find /s/vfs/dir -path "*/usr/bin/echo" | head -1))))
echo layer=$d
L=$d/usr/lib/x86_64-linux-gnu
$L/ld-linux-x86-64.so.2 --library-path $L $d/usr/bin/echo hostfolder-ok
mkdir /dev/shm/d && cp -a $d/. /dev/shm/d
M=/dev/shm/d/usr/lib/x86_64-linux-gnu
$M/ld-linux-x86-64.so.2 --library-path $M /dev/shm/d/usr/bin/echo tmpfs-ok
cp $L/libc.so.6 $M/libc.so.6.copy; cmp $L/libc.so.6 $M/libc.so.6.copy && echo cmp-ok
