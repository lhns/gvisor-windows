#!/bin/bash
# mkrootfs.sh NAME DIR OUT: builds OUT/NAME.erofs from the tree DIR (run as
# root) and the files the Windows test compares the mounted image against:
# NAME.manifest (stat of every entry), NAME.links, NAME.hashes, NAME.bigdir.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
name=$1; dir=$2; out=$3
mkdir -p "$out"
mkfs.erofs -b 4096 -T 0 "$out/$name.erofs" "$dir" >/dev/null
cd "$dir"
find . -exec stat --printf='%n\t%f\t%u\t%g\t%s\t%t\t%T\t%h\t%i\n' {} + 2>/dev/null | grep -v $'\r' > "$out/$name.manifest"
getfattr -R -h -d -m - -e hex . 2>/dev/null | awk -f "$here/xattrs.awk" > "$out/$name.xattrs" || true
find . -type l -printf '%p\t%l\n' > "$out/$name.links"
find . -type f -size -30M | sort | awk 'NR % 40 == 1' | head -700 | tr '\n' '\0' | xargs -0 -r sha256sum > "$out/$name.hashes" || true
if [ -d synth/big ]; then printf './synth/big\t%s\n' "$(ls -f synth/big | wc -l | awk '{print $1-2}')" > "$out/$name.bigdir"; fi
echo "$name: $(wc -l < "$out/$name.manifest") entries, image $(du -m "$out/$name.erofs" | cut -f1) MiB"
