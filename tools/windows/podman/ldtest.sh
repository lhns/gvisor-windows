d=$(dirname $(dirname $(dirname $(find /s/vfs/dir -path "*/usr/bin/echo" | head -1))))
echo layer=$d
L=$d/usr/lib/x86_64-linux-gnu
echo "--- from the host folder"
$L/ld-linux-x86-64.so.2 --library-path $L $d/usr/bin/echo hostfolder-ok
echo "--- from a tmpfs (/t)"
cp $L/ld-linux-x86-64.so.2 $L/libc.so.6 $d/usr/bin/echo /t/
/t/ld-linux-x86-64.so.2 --library-path /t /t/echo tmpfs-ok
echo "--- copy back onto the host folder (/u)"
cp /t/ld-linux-x86-64.so.2 /t/libc.so.6 /t/echo /u/
/u/ld-linux-x86-64.so.2 --library-path /u /u/echo copy-ok
