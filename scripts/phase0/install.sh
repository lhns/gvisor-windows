#!/usr/bin/env bash
# Installs runsc + containerd-shim-runsc-v1 (sha512 verified), podman, crun, socat and registers docker
# runtimes. Run as the CI user (uses sudo).
set -euxo pipefail
ARCH=$(uname -m)
URL=https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}
W=$(mktemp -d); cd "$W"
for f in runsc containerd-shim-runsc-v1; do
  wget -q "$URL/$f" "$URL/$f.sha512"
  sha512sum -c "$f.sha512"
  chmod a+rx "$f"; sudo mv "$f" /usr/local/bin/
done
runsc --version | tee "${RESULTS_DIR:-.}/runsc-version.txt" || true

sudo apt-get update -qq
sudo apt-get install -y -qq podman crun socat uidmap
podman --version; crun --version | head -1

# Docker runtimes. --net-raw is needed by dockerd/podman inside (gVisor docs); packet-socket-write for
# newer docker; podman's netavark may need it too. All sandboxes use systrap.
COMMON='"--platform=systrap","--net-raw","--allow-packet-socket-write","--debug-log=/tmp/runsc-logs/%ID%/","--debug","--strace=false"'
sudo mkdir -p /srv/overlay-dir /tmp/runsc-logs && sudo chmod 777 /tmp/runsc-logs
sudo tee /etc/docker/daemon.json >/dev/null <<EOF
{
  "runtimes": {
    "runsc": { "path": "/usr/local/bin/runsc", "runtimeArgs": [$COMMON] },
    "runsc-ovldir": { "path": "/usr/local/bin/runsc", "runtimeArgs": [$COMMON,"--overlay2=root:dir=/srv/overlay-dir"] }
  }
}
EOF
sudo systemctl restart docker
docker info --format '{{.Runtimes}}'
docker run --rm --runtime runsc public.ecr.aws/docker/library/alpine:3.20 sh -c 'uname -a; dmesg | head -3' || true
docker pull -q quay.io/podman/stable:latest
