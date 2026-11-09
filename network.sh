#!/usr/bin/env bash
# One-VM demo networking using a TAP and iptables. Requires root.
set -euo pipefail
TAP=tap-fcpg
GUEST_IP=172.16.0.2
GATEWAY_IP=172.16.0.1
HOST_IFACE=${HOST_IFACE:-$(ip -o -4 route show default | awk 'NR==1 {for(i=1;i<=NF;i++) if($i=="dev") {print $(i+1); exit}}')}
[[ -n "$HOST_IFACE" ]] || { echo 'Set HOST_IFACE to your outbound interface.' >&2; exit 1; }
[[ $(id -u) == 0 ]] || { echo 'Run with sudo.' >&2; exit 1; }

rule() {
  local table=$1 action=$2; shift 2
  if [[ $action == up ]]; then
    iptables -t "$table" -C "$@" 2>/dev/null || iptables -t "$table" -I "$@"
  else
    iptables -t "$table" -D "$@" 2>/dev/null || true
  fi
}

case ${1:-} in
  up)
    ip tuntap add dev "$TAP" mode tap
    ip addr add "${GATEWAY_IP}/30" dev "$TAP"
    ip link set "$TAP" up
    sysctl -w net.ipv4.ip_forward=1
    rule nat up POSTROUTING -o "$HOST_IFACE" -s "${GUEST_IP}/32" -j MASQUERADE
    rule filter up FORWARD -i "$TAP" -o "$HOST_IFACE" -j ACCEPT
    rule filter up FORWARD -i "$HOST_IFACE" -o "$TAP" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    echo "Network ready on ${TAP}; guest ${GUEST_IP}, gateway ${GATEWAY_IP}, egress ${HOST_IFACE}"
    ;;
  down)
    rule filter down FORWARD -i "$HOST_IFACE" -o "$TAP" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
    rule filter down FORWARD -i "$TAP" -o "$HOST_IFACE" -j ACCEPT
    rule nat down POSTROUTING -o "$HOST_IFACE" -s "${GUEST_IP}/32" -j MASQUERADE
    ip link del "$TAP" 2>/dev/null || true
    echo 'Demo network removed (ip_forward setting not restored).'
    ;;
  *) echo 'Usage: sudo ./network.sh up|down' >&2; exit 2 ;;
esac
