#!/usr/bin/env bash
# One matrix leg: export the image rootfs, run the static survey, then run workloads under the ptrace tracer
# inside the same image and join them with the static classification.
# Usage: run-image.sh NAME IMAGE   (needs ./tracer built, objdump, docker)
set -uxo pipefail
NAME=$1; IMG=$2
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=$PWD/results/$NAME; mkdir -p "$OUT" "$OUT/trace"
ROOT=$(mktemp -d)
EXTRA=()
if [ "$NAME" = gohello ]; then
  # static Go binary, run on top of debian
  (cd "$HERE" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/hello" hello.go)
  IMG=mirror.gcr.io/library/debian:12; docker pull -q "$IMG" >/dev/null
  cp "$OUT/hello" "$ROOT/hello"
  go version >"$OUT/go-version.txt"
  EXTRA=(-v "$OUT/hello:/hello:ro")
else
  docker pull -q "$IMG" >/dev/null
  cid=$(docker create "$IMG" true)
  docker export "$cid" | sudo tar -x -C "$ROOT" --exclude='./dev/*' --exclude='./proc/*' --exclude='./sys/*' 2>/dev/null
  docker rm "$cid" >/dev/null
  sudo chown -R "$USER" "$ROOT" 2>/dev/null; chmod -R u+rX "$ROOT" 2>/dev/null
fi
docker image inspect "$IMG" --format '{{index .RepoDigests 0}} {{.Created}}' >"$OUT/image.txt" 2>&1
python3 "$HERE/survey.py" "$ROOT" "$OUT/static" | tee "$OUT/static.txt"
sudo rm -rf "$ROOT"

trace() { # name, command (sh -c), [extra docker args]
  local n=$1 cmd=$2; shift 2
  timeout 900 docker run --rm --privileged "${EXTRA[@]}" "$@" -v "$PWD/tracer:/tracer:ro" -v "$OUT/trace:/out" \
    --entrypoint /tracer "$IMG" /out/$n.tsv -- sh -c "$cmd" >"$OUT/trace/$n.log" 2>&1
  echo "workload $n exit $?"
}
SHELLSCRIPT='for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25; do /bin/true; ls /etc >/dev/null; cat /etc/passwd | grep root >/dev/null; echo $i | sed s/1/2/ >/dev/null; done'
case "$NAME" in
  alpine) trace shell-script "$SHELLSCRIPT; apk --version" ;;
  debian) trace shell-script "$SHELLSCRIPT"; trace apt-get-update "apt-get update" ;;
  python) trace import "python3 -c 'import json,re,os,asyncio,sqlite3,decimal,ssl'"; trace shell-script "$SHELLSCRIPT" ;;
  nginx) trace nginx-start "timeout 3 nginx -g 'daemon off;'"; trace nginx-t "nginx -t" ;;
  podman) trace podman-info "podman info >/dev/null"; trace podman-version "podman version >/dev/null; crun --version >/dev/null; conmon --version >/dev/null; netavark --version >/dev/null" ;;
  gohello) trace hello "/hello"; trace hello-exec "/hello exec" ;;
esac
python3 "$HERE/join.py" "$OUT/static" "$OUT"/trace/*.tsv >"$OUT/dynamic.json" || true
cat "$OUT/dynamic.json" | head -80
