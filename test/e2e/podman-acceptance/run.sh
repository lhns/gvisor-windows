#!/usr/bin/env bash
# Podman acceptance test, Linux variant (client talks to the engine over a unix socket).
# See steps.tsv for the step list shared with the future Windows (named pipe) variant.
#
# Env (all optional unless noted):
#   CONTAINER_HOST  engine URL, e.g. unix:///tmp/podman.sock           (required)
#   DOCKER_HOST     same endpoint for the docker CLI (default: CONTAINER_HOST)
#   SHARE_DIR       directory visible at the SAME path to client and engine (bind-mount test)
#   ENGINE_CTL      command taking "restart"; must block until the engine answers again
#   NETARGS         extra args for run/build, e.g. "--network=host"
#   HOSTNET=1       containers use host net: step 7 uses httpd on 8080, step 8 uses 127.0.0.1
#   PROBE_URL       URL for the port test            (default http://127.0.0.1:8080)
#   IMG_PREFIX      registry prefix for library images (default public.ecr.aws/docker/library)
#   RESULTS_DIR     where logs go                     (default ./results)
#   ONLY            space separated step numbers to run
set -u
: "${CONTAINER_HOST:?CONTAINER_HOST required}"
export DOCKER_HOST="${DOCKER_HOST:-$CONTAINER_HOST}"
SHARE_DIR="${SHARE_DIR:-/tmp/podman-acceptance-share}"
NETARGS="${NETARGS:-}"
HOSTNET="${HOSTNET:-0}"
PROBE_URL="${PROBE_URL:-http://127.0.0.1:8080}"
IMG_PREFIX="${IMG_PREFIX:-public.ecr.aws/docker/library}"
RESULTS_DIR="${RESULTS_DIR:-./results}"
ENGINE_CTL="${ENGINE_CTL:-}"
PODMAN_BIN="${PODMAN_BIN:-podman}"
ALPINE="$IMG_PREFIX/alpine:3.20"; DEBIAN="$IMG_PREFIX/debian:12-slim"; NGINX="$IMG_PREFIX/nginx:alpine"
mkdir -p "$RESULTS_DIR" "$SHARE_DIR"
pm() { "$PODMAN_BIN" --url "$CONTAINER_HOST" "$@"; }
FAILED=0
# timeout needs an executable, so shell functions go through bash -c
t() { timeout "${STEP_TIMEOUT:-900}" "$@"; }
tp() { t "$PODMAN_BIN" --url "$CONTAINER_HOST" "$@"; }

report() { # n name rc detail
  local st=PASS; [ "$3" = 0 ] || { st=FAIL; FAILED=$((FAILED+1)); }
  echo "STEP $1 $st $2 $4" | tee -a "$RESULTS_DIR/steps.txt"
}
lastline() { tr -d '\r' < "$1" | grep -v '^\s*$' | tail -n 3 | tr '\n' '|' | cut -c1-300; }

step1() { local L=$RESULTS_DIR/step-1.log rc=0
  { tp version && tp info; } >"$L" 2>&1 || rc=1
  grep -qi 'Server' "$L" && grep -qiE 'graphDriverName|store:' "$L" || rc=1
  report 1 engine-reachable $rc "$(lastline "$L")"; }
step2() { local L=$RESULTS_DIR/step-2.log rc=0
  { tp pull -q "$ALPINE" && tp pull -q "$DEBIAN"; } >"$L" 2>&1 || rc=1
  report 2 pull $rc "$(lastline "$L")"; }
step3() { local L=$RESULTS_DIR/step-3.log rc=0 d="" out c
  out=$(tp run --rm $NETARGS "$ALPINE" echo ok 2>>"$L"); [ "$out" = ok ] || { rc=1; d="echo got '$out'; "; }
  tp run --rm $NETARGS "$ALPINE" sh -c 'exit 42' >>"$L" 2>&1; c=$?
  [ $c = 42 ] || { rc=1; d="${d}exit code $c != 42"; }
  report 3 run-exit-code $rc "${d:-ok}"; }
step4() { local L=$RESULTS_DIR/step-4.log rc=0 d=""
  command -v script >/dev/null || { report 4 interactive-tty 1 "script(1) missing"; return; }
  # In-container script prints its tty, reads a line, then waits for SIGINT (Ctrl-C = \003 on the pty).
  local inner='trap "echo GOT_INT; exit 7" INT; echo TTY=$(tty); read l; echo LINE=$l; while :; do sleep 0.2; done'
  ( sleep 10; printf 'hello\n'; sleep 3; printf '\003'; sleep 5 ) | \
    timeout 120 script -qec "$PODMAN_BIN --url $CONTAINER_HOST run -it --rm $NETARGS $ALPINE sh -c '$inner'" /dev/null >"$L" 2>&1
  grep -q 'TTY=/dev/pts' "$L" || { rc=1; d="no pty; "; }
  grep -q 'LINE=hello' "$L" || { rc=1; d="${d}input not delivered; "; }
  grep -q 'GOT_INT' "$L" || { rc=1; d="${d}SIGINT not delivered"; }
  report 4 interactive-tty $rc "${d:-ok}"; }
step5() { local L=$RESULTS_DIR/step-5.log rc=0 b; b=$(mktemp -d)
  printf 'FROM %s\nRUN apt-get update && apt-get install -y --no-install-recommends file curl\nRUN file /bin/ls && curl --version | head -1\n' "$DEBIAN" >"$b/Containerfile"
  tp build $NETARGS -t acc-build "$b" >"$L" 2>&1 || rc=1
  report 5 build-apt $rc "$(lastline "$L")"; rm -rf "$b"; }
step6() { local L=$RESULTS_DIR/step-6.log rc=0 d="" w="$SHARE_DIR/proj" cid
  rm -rf "$w"; mkdir -p "$w"; echo from-host >"$w/h.txt"
  tp run --rm $NETARGS -v "$w:/w" "$ALPINE" sh -c '
    set -e; test "$(cat /w/h.txt)" = from-host; echo from-ctr >/w/c.txt; echo r >/w/old; mv /w/old /w/new; echo d >/w/del; rm /w/del; mkdir /w/sub; echo x >/w/sub/f; sync' >"$L" 2>&1 || { rc=1; d="container script failed; "; }
  [ "$(cat "$w/c.txt" 2>/dev/null)" = from-ctr ] || { rc=1; d="${d}write not visible; "; }
  { [ -f "$w/new" ] && [ ! -e "$w/old" ]; } || { rc=1; d="${d}rename not visible; "; }
  [ ! -e "$w/del" ] || { rc=1; d="${d}delete not visible; "; }
  [ -f "$w/sub/f" ] || { rc=1; d="${d}mkdir not visible; "; }
  # a host change while a container runs must become visible inside
  cid=$(tp run -d $NETARGS -v "$w:/w" "$ALPINE" sleep 60 2>>"$L")
  if [ -n "$cid" ]; then sleep 2; echo late >"$w/late.txt"; sleep 2
    [ "$(tp exec "$cid" cat /w/late.txt 2>>"$L")" = late ] || { rc=1; d="${d}host change not visible in ctr; "; }
    tp rm -f -t 0 "$cid" >/dev/null 2>&1
  else rc=1; d="${d}could not start long-running ctr; "; fi
  report 6 bind-mount $rc "${d:-ok}"; }
step7() { local L=$RESULTS_DIR/step-7.log rc=0 cid ok=1 i
  tp rm -f -t 0 acc-web >/dev/null 2>&1
  if [ "$HOSTNET" = 1 ]; then
    cid=$(tp run -d --name acc-web --network=host "$ALPINE" httpd -f -p 8080 -h /etc 2>"$L")
  else
    cid=$(tp run -d --name acc-web -p 8080:80 "$NGINX" 2>"$L")
  fi
  [ -n "$cid" ] || rc=1
  for i in $(seq 1 30); do curl -fsS -m 3 -o /dev/null "$PROBE_URL" >>"$L" 2>&1 && { ok=0; break; }; sleep 1; done
  [ $ok = 0 ] || rc=1
  tp logs acc-web >>"$L" 2>&1; tp rm -f -t 0 acc-web >/dev/null 2>&1
  report 7 port-publish $rc "$( [ $rc = 0 ] && echo ok || lastline "$L")"; }
step8() { local L=$RESULTS_DIR/step-8.log rc=0 d target=web nm=""
  d=$(mktemp -d)
  [ "$HOSTNET" = 1 ] && { target=127.0.0.1; nm='network_mode: host'; }
  cat >"$d/compose.yaml" <<YML
services:
  web:
    image: $ALPINE
    command: ["sh","-c","mkdir -p /www && echo compose-ok > /www/index.html && httpd -f -p 80 -h /www"]
    $nm
  cli:
    image: $ALPINE
    $nm
    command: ["sh","-c","for i in 1 2 3 4 5 6 7 8 9 10; do wget -qO- http://$target:80/ && exit 0; sleep 1; done; exit 1"]
    depends_on: [web]
YML
  command -v docker >/dev/null || { report 8 compose 1 "docker CLI missing"; return; }
  t docker compose -p acc -f "$d/compose.yaml" up --abort-on-container-exit --exit-code-from cli >"$L" 2>&1 || rc=1
  grep -q compose-ok "$L" || rc=1
  docker compose -p acc -f "$d/compose.yaml" down -v >>"$L" 2>&1
  report 8 compose $rc "$(lastline "$L")"; rm -rf "$d"; }
step9() { local L=$RESULTS_DIR/step-9.log rc=0 d="" before after
  [ -n "$ENGINE_CTL" ] || { report 9 restart-persist 1 "no ENGINE_CTL"; return; }
  tp volume create accvol >"$L" 2>&1
  tp run --rm $NETARGS -v accvol:/v "$ALPINE" sh -c 'echo keep >/v/f' >>"$L" 2>&1
  before=$(tp images -q | sort -u | wc -l)
  $ENGINE_CTL restart >>"$L" 2>&1 || { rc=1; d="restart failed; "; }
  after=$(tp images -q 2>>"$L" | sort -u | wc -l)
  { [ "$after" -ge "$before" ] && [ "$before" -gt 0 ]; } || { rc=1; d="${d}images before=$before after=$after; "; }
  tp image exists "$ALPINE" || { rc=1; d="${d}alpine gone; "; }
  [ "$(tp run --rm $NETARGS -v accvol:/v "$ALPINE" cat /v/f 2>>"$L")" = keep ] || { rc=1; d="${d}volume data gone; "; }
  report 9 restart-persist $rc "${d:-images=$after volume ok}"; }
step10() { local L=$RESULTS_DIR/step-10.log rc=0 d="" pa pb ma mb n
  tp rm -f -t 0 acc-a acc-b >/dev/null 2>&1
  { tp run -d --name acc-a $NETARGS "$ALPINE" sleep 120 && tp run -d --name acc-b $NETARGS "$ALPINE" sleep 120; } >"$L" 2>&1 \
    || { report 10 isolation 1 "start failed: $(lastline "$L")"; return; }
  tp exec acc-a sh -c 'echo secret >/tmp/only-a' >>"$L" 2>&1
  pa=$(tp exec acc-a readlink /proc/self/ns/pid 2>>"$L"); pb=$(tp exec acc-b readlink /proc/self/ns/pid 2>>"$L")
  ma=$(tp exec acc-a readlink /proc/self/ns/mnt 2>>"$L"); mb=$(tp exec acc-b readlink /proc/self/ns/mnt 2>>"$L")
  { [ -n "$pa" ] && [ "$pa" != "$pb" ]; } || { rc=1; d="pid ns same/missing ($pa|$pb); "; }
  { [ -n "$ma" ] && [ "$ma" != "$mb" ]; } || { rc=1; d="${d}mnt ns same/missing ($ma|$mb); "; }
  n=$(tp exec acc-b sh -c 'ls /proc | grep -c "^[0-9]"' 2>>"$L")
  [ "${n:-99}" -le 5 ] || { rc=1; d="${d}acc-b sees $n pids; "; }
  tp exec acc-b test -e /tmp/only-a 2>>"$L" && { rc=1; d="${d}file leaked across mount ns; "; }
  tp rm -f -t 0 acc-a acc-b >/dev/null 2>&1
  report 10 isolation $rc "${d:-ok}"; }

: >"$RESULTS_DIR/steps.txt"
for n in ${ONLY:-1 2 3 4 5 6 7 8 9 10}; do "step$n" || true; done
echo "FAILED_STEPS=$FAILED" | tee -a "$RESULTS_DIR/steps.txt"
exit "$FAILED"
