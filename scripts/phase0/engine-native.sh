#!/usr/bin/env bash
# Native rootful podman engine on the runner (baseline for the acceptance test).
# Usage: engine-native.sh start|stop|restart
SOCK=${SOCK:-/tmp/podman-native.sock}
wait_ready() {
  for _ in $(seq 1 60); do
    [ -S "$SOCK" ] && sudo chmod 666 "$SOCK" && podman --url "unix://$SOCK" version >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "native engine not ready" >&2; return 1
}
case "$1" in
  start)
    sudo rm -f "$SOCK"
    sudo nohup podman system service --time=0 "unix://$SOCK" >/tmp/podman-native.log 2>&1 &
    wait_ready ;;
  stop) sudo pkill -f 'podman system service' || true; sleep 1 ;;
  restart) "$0" stop; "$0" start ;;
esac
