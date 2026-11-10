#!/usr/bin/env bash
# PROPOSED per-slot TAP/NAT helper (source only), run as root by
# firecracker-vm-network@N.service (a separate host-root oneshot, NOT inside the
# VMM unit's RootDirectory). Scope is derived from N and the operator-chosen pool
# in /etc/firecracker-vms/pool.conf:
#   tap-vmN, host PREFIX.N.1/30, guest PREFIX.N.2, NAT rules tagged "firecracker-vms".
# It refuses to run when ANY existing non-default route or interface address
# overlaps the slot's /30 (covering routes such as Docker's 172.17.0.0/16 count),
# refuses an existing interface, and removes only what its marker says it created.
# Legacy tap-fcpg / 172.16.0.0/30 and every unrelated route are never touched.
#
#   fc-vm-network check N   (no root: only the collision check; all routing tables)
#   fc-vm-network up N | down N
set -euo pipefail
PATH=/usr/sbin:/usr/bin:/sbin:/bin
readonly POOL_CONF=/etc/firecracker-vms/pool.conf
readonly COMMENT=firecracker-vms
readonly STATE_DIR=/run/firecracker-vms-net

usage() { printf 'Usage: %s check|up|down N\n' "$0" >&2; exit 2; }
die() { printf '%s\n' "$*" >&2; exit 1; }

# --- strict pool parsing (never sourced/evaluated) -----------------------------
prefix=''; max_slots=''
while IFS= read -r line || [[ -n $line ]]; do
  line=${line%%$'\r'}
  [[ -z $line || $line == \#* ]] && continue
  if [[ $line =~ ^GUEST_PREFIX=(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$ && -z $prefix ]]; then
    a=${BASH_REMATCH[1]}; b=${BASH_REMATCH[2]}
    (( a >= 1 && a <= 223 && a != 127 && b <= 255 && !(a == 169 && b == 254) )) || die "unsafe GUEST_PREFIX"
    prefix="$a.$b"
  elif [[ $line =~ ^MAX_SLOTS=([1-9][0-9]?)$ && -z $max_slots ]]; then
    max_slots=${BASH_REMATCH[1]}; (( max_slots <= 64 )) || die "MAX_SLOTS > 64"
  else
    die "invalid pool.conf line"
  fi
done < "$POOL_CONF"
[[ -n $prefix && -n $max_slots ]] || die "pool.conf needs GUEST_PREFIX and MAX_SLOTS"

[[ $# -eq 2 ]] || usage
action=$1; slot=$2
[[ $slot =~ ^(0|[1-9][0-9]?)$ ]] || usage
(( 10#$slot < max_slots )) || usage

tap="tap-vm${slot}"
net_addr="${prefix}.${slot}.0"
guest="${prefix}.${slot}.2"
host_addr="${prefix}.${slot}.1/30"
state="${STATE_DIR}/${slot}.iface"

ip2int() { local IFS=.; read -r a b c d <<< "$1"; echo $(( (a << 24) | (b << 16) | (c << 8) | d )); }

# overlaps CIDR  -> 0 when it intersects the slot /30
overlaps() {
  local cidr=$1 addr len mask slotnet
  addr=${cidr%/*}; len=32
  [[ $cidr == */* ]] && len=${cidr#*/}
  [[ $addr =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ && $len =~ ^[0-9]+$ ]] || return 1
  (( len >= 1 )) || return 1                       # 0.0.0.0/0 is the expected default route
  (( len <= 32 )) || return 1
  slotnet=$(ip2int "$net_addr")
  local eff=$(( len < 30 ? len : 30 ))
  mask=$(( (0xFFFFFFFF << (32 - eff)) & 0xFFFFFFFF ))
  (( ( $(ip2int "$addr") & mask ) == ( slotnet & mask ) ))
}

# Route types iproute2 prints before the destination (unicast is implicit).
route_types=" unicast local broadcast multicast throw unreachable prohibit blackhole nat anycast "

# check_collisions inspects EVERY routing table (VPN/policy tables included) and
# every interface address. A route is skipped only if it is a default route or one
# of this slot's own interface routes (dev tap-vmN); anything else overlapping the
# slot /30 is a collision. The destination is the first word, or the second when
# the first is a route type; "table N" annotations are ignored for the decision.
check_collisions() {
  local routes addrs dest dev i
  routes=$(ip -4 route show table all) || die "cannot list routes"
  while read -r -a f; do
    [[ ${#f[@]} -gt 0 ]] || continue
    if [[ $route_types == *" ${f[0]} "* ]]; then dest=${f[1]:-}; else dest=${f[0]}; fi
    [[ -n $dest && $dest != default ]] || continue
    dev=''
    for ((i=0;i<${#f[@]}-1;i++)); do
      if [[ ${f[i]} == dev ]]; then dev=${f[i+1]}; break; fi
    done
    [[ $dest =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(/[0-9]+)?$ ]] || continue
    [[ $dev == "$tap" ]] && continue
    if overlaps "$dest"; then die "route $dest (${f[*]}) overlaps guest subnet ${net_addr}/30; choose another GUEST_PREFIX"; fi
  done <<< "$routes"
  addrs=$(ip -4 -o addr show) || die "cannot list addresses"
  while read -r -a f; do
    for ((i=0;i<${#f[@]}-1;i++)); do
      if [[ ${f[i]} == inet && ${f[1]:-} != "$tap" ]]; then
        if overlaps "${f[i+1]}"; then die "interface address ${f[i+1]} overlaps guest subnet ${net_addr}/30"; fi
      fi
    done
  done <<< "$addrs"
}

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
  check) check_collisions ;;
  up)
    [[ $EUID == 0 ]] || die "Requires root"
    install -d -m 0755 "$STATE_DIR"
    [[ ! -e $state ]] || die "ownership marker for slot $slot exists"
    if ip link show "$tap" >/dev/null 2>&1; then die "Refusing existing $tap"; fi
    check_collisions
    [[ $(sysctl -n net.ipv4.ip_forward) == 1 ]] || die "Enable host IPv4 forwarding once during setup"
    read -ra route <<< "$(ip -4 -o route show default)"
    iface=''
    for ((i=0;i<${#route[@]}-1;i++)); do if [[ ${route[i]} == dev ]]; then iface=${route[i+1]}; break; fi; done
    [[ $iface =~ ^[a-zA-Z0-9_.:-]+$ ]] || die "Cannot find outbound interface"
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
  down)
    [[ $EUID == 0 ]] || die "Requires root"
    cleanup
    ;;
  *) usage ;;
esac
