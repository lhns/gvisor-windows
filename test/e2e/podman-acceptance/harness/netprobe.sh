#!/bin/sh
# Runs inside the podman image in a sandbox: raw rtnetlink/netfilter operations that netavark needs.
# Each line: "<cmd> => rc, output". Missing tools are installed first.
command -v ip >/dev/null && command -v nft >/dev/null && command -v iptables >/dev/null || dnf install -y -q iproute nftables iptables-nft >/dev/null 2>&1
t() { echo "### $*"; "$@" 2>&1 | head -5; echo "rc=$?"; }
sh_t() { echo "### $1"; sh -c "$1" 2>&1 | head -5; }
sh_t 'ip -V; nft --version; iptables --version'
sh_t 'ip link show'
sh_t 'ip addr show'
sh_t 'ip route show'
sh_t 'ip link add br-probe type bridge; echo rc=$?'
sh_t 'ip link show br-probe; echo rc=$?'
sh_t 'ip addr add 10.99.0.1/24 dev br-probe; echo rc=$?'
sh_t 'ip link set br-probe up; echo rc=$?'
sh_t 'ip link add vethA type veth peer name vethB; echo rc=$?'
sh_t 'ip link set vethA master br-probe; echo rc=$?'
sh_t 'ip netns add probe-ns; echo rc=$?'
sh_t 'ip link set vethB netns probe-ns; echo rc=$?'
sh_t 'ip -n probe-ns link set vethB up; echo rc=$?'
sh_t 'ip -n probe-ns addr add 10.99.0.2/24 dev vethB; echo rc=$?'
sh_t 'ip -n probe-ns route add default via 10.99.0.1; echo rc=$?'
sh_t 'ip route show; ip -n probe-ns route show'
sh_t 'ping -c1 -W2 10.99.0.2; echo rc=$?'
sh_t 'nft list ruleset; echo rc=$?'
sh_t 'nft add table inet probe; echo rc=$?'
sh_t 'nft add chain inet probe fwd "{ type filter hook forward priority 0; }"; echo rc=$?'
sh_t 'nft add table ip nat; nft add chain ip nat pre "{ type nat hook prerouting priority -100; }"; nft add rule ip nat pre tcp dport 8080 dnat to 10.99.0.2:80; echo rc=$?'
sh_t 'iptables -t nat -nL; echo rc=$?'
sh_t 'iptables -t nat -A PREROUTING -p tcp --dport 8081 -j DNAT --to-destination 10.99.0.2:80; echo rc=$?'
sh_t 'iptables -t nat -A POSTROUTING -s 10.99.0.0/24 -j MASQUERADE; echo rc=$?'
sh_t 'sysctl -w net.ipv4.ip_forward=1; cat /proc/sys/net/ipv4/ip_forward; echo rc=$?'
