#!/bin/sh
# Runs INSIDE the sandbox (docker exec): what storage/mount/net features does gVisor offer podman?
echo "== uname"; uname -a
echo "== storage.conf"; cat /etc/containers/storage.conf; env | grep -i -E 'storage|containers_' 
echo "== podman info (store)"; podman info --format '{{.Store.GraphDriverName}} {{json .Store.GraphOptions}} {{.Host.CgroupsVersion}} {{.Host.CgroupManager}} {{.Host.OCIRuntime.Name}} {{.Host.NetworkBackend}}'
echo "== mounts"; grep -E 'overlay|fuse|cgroup' /proc/mounts
echo "== processes"; ps aux 2>/dev/null | head -20
echo "== manual overlay on /var/lib/containers"
d=/var/lib/containers/probe; mkdir -p $d/l $d/u $d/w $d/m; echo lower >$d/l/f
mount -t overlay overlay -o lowerdir=$d/l,upperdir=$d/u,workdir=$d/w $d/m && { echo mount-ok; ls -la $d/m; cat $d/m/f; echo new >$d/m/g; ls $d/u; } || echo "overlay mount failed: $?"
umount $d/m 2>/dev/null
echo "== manual overlay with tmpfs upper in /tmp"
mkdir -p /tmp/o && mount -t tmpfs tmpfs /tmp/o && mkdir /tmp/o/u /tmp/o/w /tmp/o/m && mount -t overlay overlay -o lowerdir=$d/l,upperdir=/tmp/o/u,workdir=/tmp/o/w /tmp/o/m && { echo tmpfs-upper-ok; ls -la /tmp/o/m; } || echo "tmpfs-upper overlay failed"
echo "== netlink"; ls /sys/class/net 2>&1; ip link 2>&1 | head -5
echo "== fuse"; ls -la /dev/fuse 2>&1
echo "== podman run host net (verbose)"; podman --log-level=debug run --rm --network=host mirror.gcr.io/library/alpine:3.20 echo hi 2>&1 | tail -25
