#!/usr/bin/env bash
# Rootful podman + crun inside a gVisor (runsc, systrap) sandbox started through docker.
# The podman API listens on TCP inside the sandbox, published by docker, and socat turns it into a
# unix socket on the host so the acceptance test sees a plain unix-socket engine.
# Usage: engine-runsc.sh start|stop|restart
# Env: RUNTIME=runsc  STORAGE=tmpfs|hostdir  STORAGE_DRIVER=(empty=default)  SHARE_DIR  SOCK  NAME
unset DOCKER_HOST  # the test exports it for the podman socket; we need the real docker here
RUNTIME=${RUNTIME:-runsc}
STORAGE=${STORAGE:-tmpfs}
STORAGE_DRIVER=${STORAGE_DRIVER:-}
SHARE_DIR=${SHARE_DIR:-/tmp/podman-acceptance-share}
SOCK=${SOCK:-/tmp/podman-runsc.sock}
NAME=${NAME:-podman-in-runsc}
HOSTDIR=${HOSTDIR:-/srv/podman-storage}
PODMAN_IMAGE=${PODMAN_IMAGE:-quay.io/podman/stable:latest}
CGROUPS=${CGROUPS:-disabled}   # disabled | enabled (cgroupfs manager, needs runsc --in-sandbox-cgroup=v2)
# Probe knobs: SHIM=1 wraps netavark with shim.so (SHIM_SO on the host), REMOUNT_CG=1 tries rw /sys/fs/cgroup,
# INSTALL_SLIRP=1 installs slirp4netns (missing from the 5.x image).
SHIM=${SHIM:-0}; REMOUNT_CG=${REMOUNT_CG:-0}; INSTALL_SLIRP=${INSTALL_SLIRP:-0}; SHIM_SO=${SHIM_SO:-}
HERE=$(cd "$(dirname "$0")" && pwd)

start() {
  mkdir -p "$SHARE_DIR"; sudo mkdir -p "$HOSTDIR"
  local st=()
  case "$STORAGE" in
    tmpfs) st=(--tmpfs /var/lib/containers:rw,exec,mode=755) ;;  # exec: docker defaults tmpfs to noexec
    hostdir) st=(-v "$HOSTDIR:/var/lib/containers") ;;
  esac
  st+=(-e CONTAINERS_STORAGE_CONF=/etc/containers/storage.conf)  # ignore the image's fuse-overlayfs drop-ins
  [ -n "$STORAGE_DRIVER" ] && st+=(-e "STORAGE_DRIVER=$STORAGE_DRIVER")
  local pre='grep cgroup /proc/mounts'
  [ "$REMOUNT_CG" = 1 ] && pre="$pre; mount -o remount,rw /sys/fs/cgroup; grep cgroup /proc/mounts"
  [ "$INSTALL_SLIRP" = 1 ] && pre="$pre; dnf install -y -q slirp4netns"
  if [ "$SHIM" = 1 ]; then
    st+=(-v "$SHIM_SO:/opt/shim.so:ro")
    pre="$pre; NV=\$(ls /usr/libexec/podman/netavark /usr/lib/podman/netavark 2>/dev/null | head -1); mv \$NV \$NV.real; printf '#!/bin/sh\nLD_PRELOAD=/opt/shim.so exec %s.real \"\$@\"\n' \$NV >\$NV; chmod +x \$NV"
  fi
  CONF=/tmp/containers-$NAME.conf; sed "s/^cgroups = .*/cgroups = \"$CGROUPS\"/" "$HERE/containers.conf" >"$CONF"
  docker rm -f "$NAME" >/dev/null 2>&1
  docker run -d --name "$NAME" --runtime "$RUNTIME" \
    --cap-add all --security-opt seccomp=unconfined --security-opt label=disable \
    --user root --entrypoint '' \
    -v "$SHARE_DIR:$SHARE_DIR" -v "$CONF:/etc/containers/containers.conf:ro" -v "$HERE/storage.conf:/etc/containers/storage.conf:ro" \
    -p 127.0.0.1:8888:8888 -p 8080:8080 "${st[@]}" \
    "$PODMAN_IMAGE" sh -c "$pre; exec podman system service --time=0 tcp://0.0.0.0:8888" || return 1
  rm -f "$SOCK"
  # restart-looped so the socket survives a dying socat; own session so runner step cleanup keeps it
  setsid nohup bash -c "while :; do socat UNIX-LISTEN:$SOCK,unlink-early,fork,mode=666 TCP:127.0.0.1:8888; echo socat exited \$? >&2; sleep 1; done" >/tmp/socat.log 2>&1 &
  for _ in $(seq 1 120); do
    podman --url "unix://$SOCK" version >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "engine in $RUNTIME not ready" >&2; docker logs "$NAME" 2>&1 | tail -30 >&2; return 1
}
stop() { pkill -f "while :; do socat UNIX-LISTEN:$SOCK" || true; pkill -f "UNIX-LISTEN:$SOCK" || true; rm -f "$SOCK"; docker rm -f "$NAME" >/dev/null 2>&1 || true; }
case "$1" in
  start) start ;;
  stop) stop ;;
  restart) stop; start ;;   # new sandbox, same host-side storage config
esac
