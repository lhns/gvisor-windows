#!/bin/bash
# usage: boot.sh <label> <accel args...>  ; measures time until "login:" appears on serial
label=$1; shift
./qemu/qemu-system-x86_64.exe "$@" -m 512 -nographic -kernel iso/boot/vmlinuz-virt -initrd iso/boot/initramfs-virt -append "console=ttyS0" -cdrom dl/alpine-virt-3.24.4-x86_64.iso -no-reboot > "log_$label.txt" 2>&1 &
pid=$!
s=$(date +%s%N)
while kill -0 $pid 2>/dev/null; do
  if grep -q "login:" "log_$label.txt"; then e=$(date +%s%N); echo "$label: login prompt after $(( (e - s)/1000000 )) ms"; kill $pid; taskkill //F //IM qemu-system-x86_64.exe >/dev/null 2>&1; exit 0; fi
  now=$(date +%s%N); if (( (now - s)/1000000 > 240000 )); then echo "$label: no login within 240s"; kill $pid; taskkill //F //IM qemu-system-x86_64.exe >/dev/null 2>&1; exit 1; fi
  sleep 0.5
done
echo "$label: qemu exited early"; tail -3 "log_$label.txt"
