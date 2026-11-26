#!/bin/sh
# Runs in a sandbox: one nft statement per line, to find which expression netavark's ruleset trips over.
dnf install -y -q nftables >/dev/null 2>&1
t() { echo "### $1"; echo "$1" | nft -f - 2>&1 | head -3; echo "rc=$?"; }
t 'add table inet probe'
t 'add chain inet probe fw { type filter hook forward priority 0; policy accept; }'
t 'add chain inet probe post { type nat hook postrouting priority 100; policy accept; }'
t 'add chain inet probe pre { type nat hook prerouting priority -100; policy accept; }'
t 'add chain inet probe out { type nat hook output priority -100; policy accept; }'
t 'add chain inet probe c1'
t 'add rule inet probe c1 accept'
t 'add rule inet probe fw ct state established,related accept'
t 'add rule inet probe fw ip daddr 10.88.0.0/16 accept'
t 'add rule inet probe fw ip saddr 10.88.0.0/16 jump c1'
t 'add rule inet probe post ip saddr 10.88.0.0/16 masquerade'
t 'add rule inet probe post ip saddr 10.88.0.0/16 ip daddr != 224.0.0.0/4 masquerade'
t 'add rule inet probe post ip saddr 10.88.0.0/16 masquerade fully-random'
t 'add rule inet probe pre tcp dport 8080 dnat ip to 10.88.0.2:80'
t 'add rule inet probe pre fib daddr type local tcp dport 8080 dnat ip to 10.88.0.2:80'
t 'add rule inet probe out ip daddr 127.0.0.1 tcp dport 8080 dnat ip to 10.88.0.2:80'
t 'add rule inet probe post ip daddr 10.88.0.2 tcp dport 80 masquerade'
t 'add map inet probe m { type inet_service : ipv4_addr . inet_service; }'
t 'add set inet probe s { type ipv4_addr; flags interval; }'
t 'add rule inet probe fw ip saddr @s accept'
t 'add rule inet probe fw meta mark 0x2000 accept'
t 'add rule inet probe fw iifname "podman0" accept'
t 'add rule inet probe fw oifname "podman0" accept'
t 'add rule inet probe fw comment "x" accept'
nft list ruleset 2>&1 | head -60
