#!/bin/bash
# synth-xattrs.sh DIR: gives some files of the synthetic tree extended
# attributes: a file capability, a SELinux label, user.* and trusted.* values,
# enough different ones on one inode to need more than the shared area.
set -u
cd "$1"
install -m 0755 /bin/true synth/ping
setcap cap_net_raw,cap_net_bind_service+ep synth/ping || echo "setcap failed"
setfattr -n security.selinux -v 'system_u:object_r:bin_t:s0' synth/ping || echo "selinux label failed"
setfattr -n user.comment -v 'hello xattr' synth/file-0 || echo "user.comment failed"
setfattr -n trusted.custom -v 'trusted value' synth/file-0 || echo "trusted failed"
setfattr -n user.empty synth/private || echo "user.empty failed"
for i in 1 2 3 4 5 6; do
  setfattr -n "user.key$i" -v "value-$i-with-some-length-so-the-entries-are-not-tiny" synth/sub || echo "key$i failed"
done
# The same label on many files: the image keeps it in the shared area.
for i in $(seq 1 50); do
  : > "synth/labelled-$i"
  setfattr -n security.selinux -v 'system_u:object_r:usr_t:s0' "synth/labelled-$i" || true
done
getfattr -h -d -m - synth/ping synth/file-0 synth/sub 2>&1 | head -30
