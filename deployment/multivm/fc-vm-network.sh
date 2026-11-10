#!/usr/bin/env bash
# PROPOSED per-slot TAP/NAT helper (source only), run by firecracker-vm@N.service
# as ExecStartPre/ExecStopPost. Scope is derived from N alone:
#   tap-vmN, host 172.17.N.1/30, guest 172.17.N.2, NAT rules tagged "firecracker-vms".
# It refuses an existing interface/route and only removes what its marker says
# it created, so it never deletes unrelated interfaces. Legacy tap-fcpg and
# 172.16.0.0/30 are never touched.
set -euo pipefail
readonly MAX_SLOTS=4   # keep equal to fc-vm-helper.sh and vmd max_vms
readonly COMMENT=firecracker-vms
readonly STATE_DIR=/run/firecracker-vms-net

[[ $EUID == 0 ]] || { printf 'Requires root\n' >&2; exit 1; }
usage() { printf 'Usage: %s up|down N\n' "$0" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
action=$1; slot=$2
[[ $slot =~ ^(0|[1-9][0-9]?)$ ]] || usage
(( 10#$slot < MAX_SLOTS )) || usage

tap="tap-vm${slot}"
guest="172.17.${slot}.2"
host_addr="172.17.${slot}.1/30"
net="172.17.${slot}.0/30"
state="${STATE_DIR}/${slot}.iface"

cleanup() {
  [[ -f $state ]] || return 0
  read -r iface < "$state"
  iptables -D FORWARD -i "$iface" -o "$tap" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "$COMMENT" -j ACCEPT 2>/dev/null || true
  iptables -D FORWARD -i "$tap" -o "$iface" -m comment --comment "$COMMENT" -j ACCEPT 2>/dev/null || true
  iptables -t nat -D POSTROUTING -o "$iface" -s "$guest/32" -m comment --comment "$COMMENT" -j MASQUERADE 2>/dev/null || true
  ip link del "$tap" 2>/dev/null || true
  rm -f "$state"
}

case $action in
  up)
    install -d -m 0755 "$STATE_DIR"
    [[ ! -e $state ]] || { printf 'ownership marker for slot %s exists\n' "$slot" >&2; exit 1; }
    if ip link show "$tap" >/dev/null 2>&1; then printf 'Refusing existing %s\n' "$tap" >&2; exit 1; fi
    [[ -z $(ip -4 route show exact "$net") ]] || { printf 'Guest subnet already routed\n' >&2; exit 1; }
    [[ $(sysctl -n net.ipv4.ip_forward) == 1 ]] || { printf 'Enable host IPv4 forwarding once during setup\n' >&2; exit 1; }
    read -ra route <<< "$(ip -4 -o route show default)"
    iface=''
    for ((i=0;i<${#route[@]}-1;i++)); do if [[ ${route[i]} == dev ]]; then iface=${route[i+1]}; break; fi; done
    [[ $iface =~ ^[a-zA-Z0-9_.:-]+$ ]] || { printf 'Cannot find outbound interface\n' >&2; exit 1; }
    ip tuntap add dev "$tap" mode tap
    printf '%s\n' "$iface" > "$state"
    trap cleanup ERR
    ip addr add "$host_addr" dev "$tap"
    ip link set "$tap" up
    iptables -t nat -I POSTROUTING -o "$iface" -s "$guest/32" -m comment --comment "$COMMENT" -j MASQUERADE
    iptables -I FORWARD -i "$tap" -o "$iface" -m comment --comment "$COMMENT" -j ACCEPT
    iptables -I FORWARD -i "$iface" -o "$tap" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "$COMMENT" -j ACCEPT
    trap - ERR
    ;;
  down) cleanup ;;
  *) usage ;;
esac
