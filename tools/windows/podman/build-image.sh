#!/usr/bin/env bash
# Builds the erofs root image of the Podman engine (start-engine.ps1 -Image) from quay.io/podman/stable.
# Usage: build-image.sh <podman-version> <out.erofs>   (Ubuntu runner: needs sudo, podman, apt)
set -euo pipefail
ver=$1 out=$2
h=$(cd "$(dirname "$0")/../../.." && pwd)/test/e2e/podman-acceptance/harness
sudo apt-get update -qq && sudo apt-get install -y -qq erofs-utils
podman pull -q quay.io/podman/stable:v${ver}
c=$(podman create quay.io/podman/stable:v${ver} sh)
sudo rm -rf /tmp/root && mkdir /tmp/root
podman export "$c" | sudo tar -x -C /tmp/root --numeric-owner --same-owner -p --xattrs --xattrs-include="*"
# Engine settings baked into the read-only image (same files as the Linux harness; the
# sqlite database goes to a tmpfs until sqlite works on a host folder, see the results).
sudo mkdir -p /tmp/root/etc/containers /tmp/root/run /tmp/root/tmp /tmp/root/var/lib/containers
sudo cp $h/containers.conf /tmp/root/etc/containers/containers.conf
sudo sed 's/^driver = .*/driver = "vfs"/' $h/storage.conf | sudo tee /tmp/root/etc/containers/storage.conf >/dev/null
# Marker that makes Podman ask the gateway to expose published ports (libpod/networking_machine.go);
# the windows-nat gateway answers at gateway.containers.internal (10.0.2.2:80).
echo qemu | sudo tee /tmp/root/etc/containers/podman-machine >/dev/null
echo "10.0.2.2 gateway.containers.internal host.containers.internal" | sudo tee -a /tmp/root/etc/hosts >/dev/null
echo "nameserver 10.0.2.3" | sudo tee /tmp/root/etc/resolv.conf >/dev/null
sudo mkfs.erofs -b 4096 -T 0 "$out" /tmp/root >/dev/null
ls -l "$out"
