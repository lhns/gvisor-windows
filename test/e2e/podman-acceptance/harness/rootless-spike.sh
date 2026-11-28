#!/usr/bin/env bash
# Spike: Podman engine inside an unprivileged, rootless runsc sandbox (no docker, no root).
# Run as the unprivileged user. Usage: rootless-spike.sh start|stop|restart
# Env: GV (dir with runsc + gvisor-bin)  ROOTFS (unpacked engine image)  WORK  NET=host|windows-nat|sandbox|none
#      STORAGE_DRIVER=overlay|vfs  SOCK (unix socket for the client)  SHARE_DIR
set -u
GV=${GV:?}; ROOTFS=${ROOTFS:?}; WORK=${WORK:?}; NET=${NET:-host}; STORAGE_DRIVER=${STORAGE_DRIVER:-overlay}
export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-$HOME/xdg}; mkdir -p "$XDG_RUNTIME_DIR"; chmod 700 "$XDG_RUNTIME_DIR"
SOCK=${SOCK:-$HOME/podman.sock}; SHARE_DIR=${SHARE_DIR:-$HOME/share}
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$WORK"/{state,logs,storage,bundle} "$SHARE_DIR"
RUNSC=("$GV/runsc" --rootless --ignore-cgroups --network="$NET" --platform=systrap --overlay2=root:memory
       --in-sandbox-cgroup=v2 --root "$WORK/state" --debug --debug-log="$WORK/logs/")
[ "$NET" = windows-nat ] && RUNSC+=(--publish=8888:8888,8080:80)

mkspec() {
  mkdir -p "$ROOTFS/etc/containers" "$ROOTFS/run" "$ROOTFS/tmp" "$ROOTFS/var/lib/containers" "$ROOTFS/mnt"
  cp "$HERE/containers.conf" "$ROOTFS/etc/containers/containers.conf"
  sed "s/^driver = .*/driver = \"$STORAGE_DRIVER\"/" "$HERE/storage.conf" >"$ROOTFS/etc/containers/storage.conf"
  if [ "$NET" = windows-nat ]; then
    echo qemu >"$ROOTFS/etc/containers/podman-machine"
    echo "10.0.2.2 gateway.containers.internal host.containers.internal" >>"$ROOTFS/etc/hosts"
    echo "nameserver 10.0.2.3" >"$ROOTFS/etc/resolv.conf"
  fi
  python3 - "$ROOTFS" "$WORK" "$SHARE_DIR" "$STORAGE_DRIVER" "$NET" >"$WORK/bundle/config.json" <<'PY'
import json, sys
root, work, share, drv, net = sys.argv[1:6]
caps = ['CAP_'+c for c in "CHOWN DAC_OVERRIDE DAC_READ_SEARCH FOWNER FSETID KILL SETGID SETUID SETPCAP LINUX_IMMUTABLE NET_BIND_SERVICE NET_BROADCAST NET_ADMIN NET_RAW IPC_LOCK IPC_OWNER SYS_MODULE SYS_RAWIO SYS_CHROOT SYS_PTRACE SYS_PACCT SYS_ADMIN SYS_BOOT SYS_NICE SYS_RESOURCE SYS_TIME SYS_TTY_CONFIG MKNOD LEASE AUDIT_WRITE AUDIT_CONTROL SETFCAP MAC_OVERRIDE MAC_ADMIN SYSLOG WAKE_ALARM BLOCK_SUSPEND AUDIT_READ PERFMON BPF CHECKPOINT_RESTORE".split()]
def m(d, t, s, o=None):
    x = {"destination": d, "type": t, "source": s}
    if o: x["options"] = o
    return x
mounts = [m('/proc','proc','proc'), m('/dev','tmpfs','tmpfs',['nosuid','mode=755','size=65536k']),
  m('/dev/pts','devpts','devpts',['nosuid','noexec','newinstance','ptmxmode=0666','mode=0620']),
  m('/dev/shm','tmpfs','shm',['nosuid','noexec','nodev','mode=1777','size=65536k']),
  m('/sys','sysfs','sysfs',['nosuid','noexec','nodev','ro']),
  m('/sys/fs/cgroup','cgroup2','cgroup2',['nosuid','noexec','nodev']),
  m('/run','tmpfs','tmpfs',['mode=755']), m('/tmp','tmpfs','tmpfs',['mode=1777']), m('/var/tmp','tmpfs','tmpfs',['mode=1777']),
  m('/var/lib/containers','bind',work+'/storage',['rbind','rw']),
  m(share,'bind',share,['rbind','rw'])]
probe = '{ grep -E "^(Uid|Gid|Cap)" /proc/self/status; cat /proc/self/uid_map; uname -a; } > /var/lib/containers/engine-env.txt 2>&1; rm -f /usr/bin/fuse-overlayfs /usr/sbin/fuse-overlayfs; '
spec = {"ociVersion":"1.0.2",
 "process":{"terminal":False,"user":{"uid":0,"gid":0},
  "args":["sh","-c",probe+"mkdir -p /run/podman /run/containers/storage; exec podman --log-level=info system service --time=0 tcp://0.0.0.0:8888"],
  "env":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin","HOME=/root","TERM=xterm","container=podman","CONTAINERS_STORAGE_CONF=/etc/containers/storage.conf","STORAGE_DRIVER="+drv],
  "cwd":"/","capabilities":{"bounding":caps,"effective":caps,"permitted":caps,"inheritable":caps}},
 "root":{"path":root,"readonly":False},"hostname":"podman-spike","mounts":mounts,
 "linux":{"namespaces":[{"type":t} for t in ("pid","ipc","uts","mount") if True]
   + ([{"type":"network"}] if net in ("sandbox","none") else []),
  "devices":[{"path":"/dev/"+n,"type":"c","major":a,"minor":b,"fileMode":438,"uid":0,"gid":0} for n,a,b in
   (("null",1,3),("zero",1,5),("full",1,7),("random",1,8),("urandom",1,9),("tty",5,0))]}}
print(json.dumps(spec, indent=1))
PY
}

stop() {
  pkill -f "UNIX-LISTEN:$SOCK" || true; rm -f "$SOCK"
  "${RUNSC[@]}" kill podman-spike KILL >/dev/null 2>&1 || true
  [ -f "$WORK/runsc.pid" ] && kill "$(cat "$WORK/runsc.pid")" 2>/dev/null
  sleep 2; "${RUNSC[@]}" delete --force podman-spike >/dev/null 2>&1 || true
}
start() {
  mkspec || return 1
  echo "start: ${RUNSC[*]} run --bundle $WORK/bundle podman-spike"
  setsid nohup "${RUNSC[@]}" run --bundle "$WORK/bundle" podman-spike >>"$WORK/runsc.out" 2>&1 &
  echo $! >"$WORK/runsc.pid"
  setsid nohup bash -c "while :; do socat UNIX-LISTEN:$SOCK,unlink-early,fork,mode=600 TCP:127.0.0.1:8888; sleep 1; done" >"$WORK/socat.log" 2>&1 &
  for _ in $(seq 1 120); do
    podman --url "unix://$SOCK" version >/dev/null 2>&1 && { echo "engine up"; return 0; }
    kill -0 "$(cat "$WORK/runsc.pid")" 2>/dev/null || { echo "runsc exited"; break; }
    sleep 1
  done
  echo "engine not ready; runsc.out:"; tail -40 "$WORK/runsc.out"
  echo "--- diag"; ss -ltnp 2>&1 | head; pgrep -a socat; cat "$WORK/socat.log"
  curl -sS -m 5 -v http://127.0.0.1:8888/_ping 2>&1 | tail -15
  podman --url "unix://$SOCK" version 2>&1 | tail -15
  return 1
}
case "${1:-}" in start) start ;; stop) stop ;; restart) stop; start ;; esac
