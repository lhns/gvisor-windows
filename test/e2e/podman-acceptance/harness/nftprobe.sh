#!/bin/sh
# Runs in a sandbox: one nft statement per line, mirroring the rules netavark 2 sends (see nft-in.log of the
# bridge job), to find which expression gVisor's nftables rejects.
dnf install -y -q nftables >/dev/null 2>&1
nft add table inet p 2>&1
for c in INPUT FORWARD; do nft "add chain inet p $c { type filter hook $(echo $c | tr A-Z a-z) priority 0; policy accept; }" 2>&1; done
nft 'add chain inet p POSTROUTING { type nat hook postrouting priority 100; policy accept; }' 2>&1
nft 'add chain inet p PREROUTING { type nat hook prerouting priority -100; policy accept; }' 2>&1
nft 'add chain inet p OUTPUT { type nat hook output priority -100; policy accept; }' 2>&1
for c in DNAT SETMARK ISO1 C16; do nft "add chain inet p $c" 2>&1; done
t() { echo "### $*"; nft "$@" 2>&1 | head -4; }
t add rule inet p POSTROUTING meta mark and 0x2000 == 0x2000 masquerade
t add rule inet p SETMARK meta mark set meta mark or 0x2000
t add rule inet p PREROUTING fib daddr type local jump DNAT
t add rule inet p FORWARD ct state invalid drop
t add rule inet p FORWARD ct mark and 0x1000 == 0x1000 accept
t add rule inet p ISO1 iifname '"podman0"' oifname != '"podman0"' jump C16
t add rule inet p C16 ip daddr 10.88.0.0/16 accept
t add rule inet p C16 ip daddr != 224.0.0.0/4 masquerade
t add rule inet p INPUT ip saddr 10.88.0.0/16 meta l4proto '{ udp, tcp }' th dport 53 accept
t add rule inet p INPUT ip saddr 10.88.0.0/16 meta l4proto udp th dport 53 accept
t add rule inet p FORWARD ip daddr 10.88.0.0/16 ct state '{ established, related }' accept
t add rule inet p FORWARD ip daddr 10.88.0.0/16 ct state established,related accept
t add rule inet p DNAT tcp dport 8080 dnat ip to 10.88.0.2:80
t add rule inet p DNAT ip saddr 10.88.0.0/16 tcp dport 8080 jump SETMARK
nft -a list ruleset 2>&1 | head -70
