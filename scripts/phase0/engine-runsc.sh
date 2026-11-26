#!/usr/bin/env bash
# Rootful podman + crun inside a gVisor (runsc, systrap) sandbox started through docker.
# The podman API listens on TCP inside the sandbox, published by docker, and socat turns it into a
# unix socket on the host so the acceptance test sees a plain unix-socket engine.
# Usage: engine-runsc.sh start|stop|restart
# Env: RUNTIME=runsc  STORAGE=tmpfs|hostdir  STORAGE_DRIVER=(empty=default)  SHARE_DIR  SOCK  NAME
RUNTIME=${RUNTIME:-runsc}
STORAGE=${STORAGE:-tmpfs}
STORAGE_DRIVER=${STORAGE_DRIVER:-}
SHARE_DIR=${SHARE_DIR:-/tmp/podman-acceptance-share}
SOCK=${SOCK:-/tmp/podman-runsc.sock}
NAME=${NAME:-podman-in-runsc}
HOSTDIR=${HOSTDIR:-/srv/podman-storage}
HERE=$(cd "$(dirname "$0")" && pwd)

start() {
  mkdir -p "$SHARE_DIR"; sudo mkdir -p "$HOSTDIR"
  local st=()
  case "$STORAGE" in
    tmpfs) st=(--tmpfs /var/lib/containers) ;;
    hostdir) st=(-v "$HOSTDIR:/var/lib/containers") ;;
  esac
  [ -n "$STORAGE_DRIVER" ] && st+=(-e "STORAGE_DRIVER=$STORAGE_DRIVER")
  docker rm -f "$NAME" >/dev/null 2>&1
  docker run -d --name "$NAME" --runtime "$RUNTIME" \
    --cap-add all --security-opt seccomp=unconfined --security-opt label=disable \
    --user root --entrypoint '' \
    -v "$SHARE_DIR:$SHARE_DIR" -v "$HERE/containers.conf:/etc/containers/containers.conf:ro" -v "$HERE/storage.conf:/etc/containers/storage.conf:ro" \
    -p 127.0.0.1:8888:8888 -p 8080:8080 "${st[@]}" \
    quay.io/podman/stable:latest podman system service --time=0 tcp://0.0.0.0:8888 || return 1
  rm -f "$SOCK"
  nohup socat "UNIX-LISTEN:$SOCK,fork,mode=666" TCP:127.0.0.1:8888 >/tmp/socat.log 2>&1 &
  for _ in $(seq 1 120); do
    podman --url "unix://$SOCK" version >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "engine in $RUNTIME not ready" >&2; docker logs "$NAME" 2>&1 | tail -30 >&2; return 1
}
stop() { pkill -f "UNIX-LISTEN:$SOCK" || true; docker rm -f "$NAME" >/dev/null 2>&1 || true; }
case "$1" in
  start) start ;;
  stop) stop ;;
  restart) stop; start ;;   # new sandbox, same host-side storage config
esac
