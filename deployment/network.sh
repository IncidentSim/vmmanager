#!/usr/bin/env bash
# Root-owned fixed-scope networking helper, invoked only by systemd.
set -euo pipefail
TAP=tap-fcpg
GUEST=172.16.0.2
STATE=/run/firecracker-builder-network.iface
COMMENT=firecracker-builder
[[ $EUID == 0 ]] || { printf 'Requires root\n' >&2; exit 1; }
cleanup() {
  [[ -f $STATE ]] || return 0
  read -r iface < "$STATE"
  iptables -D FORWARD -i "$iface" -o "$TAP" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "$COMMENT" -j ACCEPT 2>/dev/null || true
  iptables -D FORWARD -i "$TAP" -o "$iface" -m comment --comment "$COMMENT" -j ACCEPT 2>/dev/null || true
  iptables -t nat -D POSTROUTING -o "$iface" -s "$GUEST/32" -m comment --comment "$COMMENT" -j MASQUERADE 2>/dev/null || true
  ip link del "$TAP" 2>/dev/null || true
  rm -f "$STATE"
}
case ${1:-} in
 up)
  [[ ! -e $STATE ]] || { printf 'Network ownership marker already exists\n' >&2; exit 1; }
  if ip link show "$TAP" >/dev/null 2>&1; then printf 'Refusing existing %s\n' "$TAP" >&2; exit 1; fi
  [[ -z $(ip -4 route show exact 172.16.0.0/30) ]] || { printf 'Guest subnet already routed\n' >&2; exit 1; }
  [[ $(sysctl -n net.ipv4.ip_forward) == 1 ]] || { printf 'Enable host IPv4 forwarding once during setup\n' >&2; exit 1; }
  read -ra route <<< "$(ip -4 -o route show default)"
  iface=''
  for ((i=0;i<${#route[@]}-1;i++)); do if [[ ${route[i]} == dev ]]; then iface=${route[i+1]}; break; fi; done
  [[ $iface =~ ^[a-zA-Z0-9_.:-]+$ ]] || { printf 'Cannot find outbound interface\n' >&2; exit 1; }
  ip tuntap add dev "$TAP" mode tap
  printf '%s\n' "$iface" > "$STATE"
  trap cleanup ERR
  ip addr add 172.16.0.1/30 dev "$TAP"
  ip link set "$TAP" up
  iptables -t nat -I POSTROUTING -o "$iface" -s "$GUEST/32" -m comment --comment "$COMMENT" -j MASQUERADE
  iptables -I FORWARD -i "$TAP" -o "$iface" -m comment --comment "$COMMENT" -j ACCEPT
  iptables -I FORWARD -i "$iface" -o "$TAP" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "$COMMENT" -j ACCEPT
  trap - ERR
  ;;
 down) cleanup ;;
 *) printf 'Usage: %s up|down\n' "$0" >&2; exit 2 ;;
esac
