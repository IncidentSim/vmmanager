#!/usr/bin/env bash
# Source-only host-root TAP/firewall helper. Never run in a VMM jail.
# check is unprivileged; refresh updates an existing TAP without link/address
# changes, VMM restart or conntrack deletion. See NETWORK.md before activation.
# Rules/chains belong exclusively to slot N; no global flush or policy changes.
set -euo pipefail
PATH=/usr/sbin:/usr/bin:/sbin:/bin
readonly POOL_CONF=/etc/firecracker-vms/pool.conf
readonly COMMENT=firecracker-vms
readonly STATE_DIR=/run/firecracker-vms-net

usage() { printf 'Usage: %s check|up|refresh|down|flush-conntrack N\n' "$0" >&2; exit 2; }
die() { printf '%s\n' "$*" >&2; exit 1; }

# --- strict pool parsing (never sourced/evaluated) -----------------------------
# Bash read discards NUL bytes; reject them rather than accepting a different
# configuration than the Go parser sees. No caller-controlled path is accepted.
LC_ALL=C
export LC_ALL
tr -d '\000' < "$POOL_CONF" | cmp -s "$POOL_CONF" - || die "pool.conf contains NUL or is unreadable"
# Same reserved destination set as ValidatePublicIPv4 in internal/vmhelper.
blocked=(0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16
         172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 192.168.0.0/16
         198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 224.0.0.0/3)
valid_public() {
  local value=$1 a b c d oct n addr len mask
  [[ $value =~ ^(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$ ]] || return 1
  local IFS=.; read -r a b c d <<< "$value"
  for oct in "$a" "$b" "$c" "$d"; do (( oct <= 255 )) || return 1; done
  n=$(( (a << 24) | (b << 16) | (c << 8) | d ))
  for cidr in "${blocked[@]}"; do
    addr=${cidr%/*}; len=${cidr#*/}; read -r a b c d <<< "$addr"
    mask=$(( (0xFFFFFFFF << (32-len)) & 0xFFFFFFFF ))
    (( (n & mask) != (((a << 24) | (b << 16) | (c << 8) | d) & mask) )) || return 1
  done
}
prefix=''; max_slots=''; max_slot_cpu=''; public=''; port_base=''
while IFS= read -r line || [[ -n $line ]]; do
  line=${line%%$'\r'}
  [[ -z $line || $line == \#* ]] && continue
  if [[ $line =~ ^GUEST_PREFIX=(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$ && -z $prefix ]]; then
    a=${BASH_REMATCH[1]}; b=${BASH_REMATCH[2]}
    (( a >= 1 && a <= 223 && a != 127 && b <= 255 && !(a == 169 && b == 254) )) || die "unsafe GUEST_PREFIX"
    prefix="$a.$b"
  elif [[ $line =~ ^MAX_SLOTS=([1-9][0-9]?)$ && -z $max_slots ]]; then
    max_slots=${BASH_REMATCH[1]}; (( max_slots <= 64 )) || die "MAX_SLOTS > 64"
  elif [[ $line =~ ^MAX_SLOT_CPU_MILLI=([1-9][0-9]{0,4})$ && -z $max_slot_cpu ]]; then
    max_slot_cpu=${BASH_REMATCH[1]}
    (( max_slot_cpu <= 64000 && max_slot_cpu % 10 == 0 )) || die "invalid MAX_SLOT_CPU_MILLI"
  elif [[ $line == PUBLIC_IPV4=* && -z $public ]]; then
    public=${line#*=}; valid_public "$public" || die "invalid PUBLIC_IPV4"
  elif [[ $line =~ ^SSH_PORT_BASE=([1-9][0-9]{3,4})$ && -z $port_base ]]; then
    port_base=${BASH_REMATCH[1]}
    (( port_base >= 1024 && port_base <= 65535 )) || die "invalid SSH_PORT_BASE"
  else
    die "invalid pool.conf line"
  fi
done < "$POOL_CONF"
[[ -n $prefix && -n $max_slots ]] || die "pool.conf needs GUEST_PREFIX and MAX_SLOTS"
[[ ( -z $public && -z $port_base ) || ( -n $public && -n $port_base ) ]] || die "PUBLIC_IPV4 and SSH_PORT_BASE must be paired"
if [[ -n $port_base ]]; then
  (( port_base + max_slots - 1 <= 65535 )) || die "public port range exceeds 65535"
fi

[[ $# -eq 2 ]] || usage
action=$1; slot=$2
[[ $slot =~ ^(0|[1-9][0-9]?)$ ]] || usage
(( 10#$slot < max_slots )) || usage

tap="tap-vm${slot}"
net_addr="${prefix}.${slot}.0"
guest="${prefix}.${slot}.2"
host_addr="${prefix}.${slot}.1/30"
state="${STATE_DIR}/${slot}.iface"
fc="FCVM${slot}F"; ic="FCVM${slot}I"; nc="FCVM${slot}N"; mc="FCVM${slot}M"; v6c="FCVM${slot}V6"
tag="${COMMENT}:slot${slot}"
port=''; [[ -z $port_base ]] || port=$(( port_base + slot ))

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

# Hook insertion/deletion touches only exact, commented rules. Restores below
# use --noflush and explicitly flush ONLY our named chains, atomically per table.
hook() {
  local tool=$1 table=$2 base=$3 exists=0; shift 3
  if "$tool" -w -t "$table" -C "$base" "$@" 2>/dev/null; then exists=1; fi
  {
    printf '*%s\n' "$table"
    if (( exists )); then
      printf -- '-D %s' "$base"; printf ' %s' "$@"; printf '\n'
    fi
    # Delete/reinsert atomically, so refresh also repairs hook precedence after
    # another firewall manager inserted a permissive rule ahead of our jump.
    printf -- '-I %s' "$base"; printf ' %s' "$@"; printf '\nCOMMIT\n'
  } | "${tool}-restore" -w --noflush
}
unhook() {
  local tool=$1 table=$2 base=$3; shift 3
  while "$tool" -w -t "$table" -C "$base" "$@" 2>/dev/null; do
    "$tool" -w -t "$table" -D "$base" "$@"
  done
}
find_external() {
  local routes i; routes=$(ip -4 -o route show default)
  # Ambiguous multi-default/policy configurations need an operator review, not
  # silently selecting a VPN or a different uplink.
  [[ -n $routes && $routes != *$'\n'* ]] || die "need exactly one IPv4 default route"
  read -ra route <<< "$routes"
  iface=''
  for ((i=0;i<${#route[@]}-1;i++)); do
    if [[ ${route[i]} == dev ]]; then iface=${route[i+1]}; break; fi
  done
  [[ $iface =~ ^[a-zA-Z0-9_.:-]+$ && $iface != "$tap" ]] || die "Cannot find outbound interface"
  if [[ -n $public ]]; then
    local addresses; addresses=$(ip -4 -o addr show)
    local found=0
    while read -ra fields; do
      for ((i=0;i<${#fields[@]}-1;i++)); do
        if [[ ${fields[i]} == inet && ${fields[i+1]%/*} == "$public" && ${fields[1]} == "$iface" ]]; then found=1; fi
      done
    done <<< "$addresses"
    (( found == 1 )) || die "PUBLIC_IPV4 must be assigned to the default-route interface"
  fi
}
verify_tap() {
  local details addresses row matched=0
  details=$(ip tuntap show dev "$tap")
  # Some iproute2 versions ignore the dev filter and list every TUN/TAP.
  # Validate the exact requested interface, not the first listed device.
  while IFS= read -r row; do
    if [[ $row == "$tap:"* ]]; then
      [[ $row == "$tap: tap" || $row == "$tap: tap "* ]] || die "existing interface is not the expected TAP"
      matched=$((matched + 1))
    fi
  done <<< "$details"
  (( matched == 1 )) || die "existing interface is not the expected TAP"
  addresses=$(ip -4 -o addr show dev "$tap")
  [[ $addresses == *" inet $host_addr "* ]] || die "existing TAP lacks expected $host_addr; not modifying it"
}
flush_endpoint() {
  [[ -n $public ]] || return 0
  local rc=0 output
  output=$(conntrack -D -f ipv4 -p tcp --orig-dst "$public" --dport "$port" 2>&1) || rc=$?
  # Exit 1 also covers permission/backend errors. Accept it ONLY with the
  # tool's explicit zero-deletions diagnostic, never blindly as "no matches".
  if (( rc != 0 )); then
    [[ $rc == 1 && $output == *": 0 flow entries have been deleted."* ]] || die "endpoint conntrack deletion failed: $output"
  fi
}
apply_rules() {
  # No IPv6 opt-out exists: all TAP IPv6 INPUT/OUTPUT/FORWARD is blocked.
  ip6tables-nft-restore -w --noflush <<EOF
*filter
:$v6c - [0:0]
-F $v6c
-A $v6c -j DROP
COMMIT
EOF
  hook ip6tables-nft filter INPUT -i "$tap" -m comment --comment "$tag" -j "$v6c"
  hook ip6tables-nft filter OUTPUT -o "$tap" -m comment --comment "$tag" -j "$v6c"
  hook ip6tables-nft filter FORWARD -i "$tap" -m comment --comment "$tag" -j "$v6c"
  hook ip6tables-nft filter FORWARD -o "$tap" -m comment --comment "$tag" -j "$v6c"

  # A private external SSH client must still receive replies. Only the REPLY
  # direction of this exact public DNAT endpoint bypasses destination blocks.
  {
    printf '*filter\n:%s - [0:0]\n:%s - [0:0]\n-F %s\n-F %s\n' "$fc" "$ic" "$fc" "$ic"
    printf -- '-A %s -i %s ! -s %s/32 -j DROP\n' "$fc" "$tap" "$guest"
    printf -- '-A %s -m conntrack --ctstate INVALID -j DROP\n' "$fc"
    if [[ -n $public ]]; then
      printf -- '-A %s -i %s -o %s -s %s/32 -p tcp --sport 22 -m conntrack --ctstate DNAT --ctdir REPLY --ctorigdst %s --ctorigdstport %s -j ACCEPT\n' "$fc" "$tap" "$iface" "$guest" "$public" "$port"
    fi
    for cidr in "${blocked[@]}"; do
      printf -- '-A %s -i %s -d %s -j DROP\n' "$fc" "$tap" "$cidr"
    done
    # Global-address VPN/policy routes are also denied (not just RFC1918).
    local routes dest dev i
    routes=$(ip -4 route show table all)
    while read -ra f; do
      [[ ${#f[@]} -gt 0 ]] || continue
      dest=${f[0]}; [[ $route_types != *" $dest "* ]] || dest=${f[1]:-}
      [[ $dest != default && $dest != 0.0.0.0/0 && $dest =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+(/[0-9]+)?$ ]] || continue
      dev=''
      for ((i=0;i<${#f[@]}-1;i++)); do [[ ${f[i]} != dev ]] || dev=${f[i+1]}; done
      [[ -z $dev || $dev == "$iface" ]] && continue
      printf -- '-A %s -i %s -d %s -j DROP\n' "$fc" "$tap" "$dest"
    done <<< "$routes"
    printf -- '-A %s -i %s -o %s -j ACCEPT\n' "$fc" "$tap" "$iface"
    printf -- '-A %s -i %s -o %s -d %s/32 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT\n' "$fc" "$iface" "$tap" "$guest"
    if [[ -n $public ]]; then
      printf -- '-A %s -i %s -o %s -d %s/32 -p tcp --dport 22 -m conntrack --ctstate DNAT --ctdir ORIGINAL --ctorigdst %s --ctorigdstport %s -j ACCEPT\n' "$fc" "$iface" "$tap" "$guest" "$public" "$port"
    fi
    printf -- '-A %s -j DROP\n' "$fc"
    printf -- '-A %s ! -s %s/32 -j DROP\n' "$ic" "$guest"
    printf -- '-A %s -m conntrack --ctstate ESTABLISHED,RELATED --ctdir REPLY -j ACCEPT\n' "$ic"
    printf -- '-A %s -j DROP\nCOMMIT\n' "$ic"
  } | iptables-nft-restore -w --noflush
  hook iptables-nft filter INPUT -i "$tap" -m comment --comment "$tag" -j "$ic"
  hook iptables-nft filter FORWARD -i "$tap" -m comment --comment "$tag" -j "$fc"
  hook iptables-nft filter FORWARD -o "$tap" -m comment --comment "$tag" -j "$fc"
  {
    printf '*nat\n:%s - [0:0]\n:%s - [0:0]\n-F %s\n-F %s\n' "$nc" "$mc" "$nc" "$mc"
    if [[ -n $public ]]; then
      printf -- '-A %s -i %s -d %s/32 -p tcp --dport %s -j DNAT --to-destination %s:22\n' "$nc" "$iface" "$public" "$port" "$guest"
    fi
    printf -- '-A %s -o %s -s %s/32 -j MASQUERADE\nCOMMIT\n' "$mc" "$iface" "$guest"
  } | iptables-nft-restore -w --noflush
  hook iptables-nft nat PREROUTING -m comment --comment "$tag" -j "$nc"
  hook iptables-nft nat POSTROUTING -s "$guest/32" -m comment --comment "$tag" -j "$mc"
}
remove_legacy() {
  # Migrate only the three exact rules emitted by the old version, never an
  # unrelated broad rule. Legacy marker contains only its original uplink.
  [[ -f $state ]] || return 0
  local old; read -r old < "$state"
  [[ $old =~ ^[a-zA-Z0-9_.:-]+$ ]] || die "invalid ownership marker"
  unhook iptables-nft filter FORWARD -i "$old" -o "$tap" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment "$COMMENT" -j ACCEPT
  unhook iptables-nft filter FORWARD -i "$tap" -o "$old" -m comment --comment "$COMMENT" -j ACCEPT
  unhook iptables-nft nat POSTROUTING -o "$old" -s "$guest/32" -m comment --comment "$COMMENT" -j MASQUERADE
}
remove_rules() {
  unhook iptables-nft nat PREROUTING -m comment --comment "$tag" -j "$nc"
  unhook iptables-nft nat POSTROUTING -s "$guest/32" -m comment --comment "$tag" -j "$mc"
  unhook iptables-nft filter INPUT -i "$tap" -m comment --comment "$tag" -j "$ic"
  unhook iptables-nft filter FORWARD -i "$tap" -m comment --comment "$tag" -j "$fc"
  unhook iptables-nft filter FORWARD -o "$tap" -m comment --comment "$tag" -j "$fc"
  unhook ip6tables-nft filter INPUT -i "$tap" -m comment --comment "$tag" -j "$v6c"
  unhook ip6tables-nft filter OUTPUT -o "$tap" -m comment --comment "$tag" -j "$v6c"
  unhook ip6tables-nft filter FORWARD -i "$tap" -m comment --comment "$tag" -j "$v6c"
  unhook ip6tables-nft filter FORWARD -o "$tap" -m comment --comment "$tag" -j "$v6c"
  local chain
  for chain in "$fc" "$ic"; do
    iptables-nft -w -t filter -F "$chain" 2>/dev/null || true
    iptables-nft -w -t filter -X "$chain" 2>/dev/null || true
  done
  for chain in "$nc" "$mc"; do
    iptables-nft -w -t nat -F "$chain" 2>/dev/null || true
    iptables-nft -w -t nat -X "$chain" 2>/dev/null || true
  done
  ip6tables-nft -w -t filter -F "$v6c" 2>/dev/null || true
  ip6tables-nft -w -t filter -X "$v6c" 2>/dev/null || true
}

case $action in
  check) check_collisions; exit 0 ;;
  up|refresh|down|flush-conntrack) [[ $EUID == 0 ]] || die "Requires root" ;;
  *) usage ;;
esac
install -d -m 0755 "$STATE_DIR"
exec 9>"$STATE_DIR/.lock"
flock -x 9
case $action in
  up|refresh)
    check_collisions
    [[ $(sysctl -n net.ipv4.ip_forward) == 1 ]] || die "Enable host IPv4 forwarding once during setup"
    find_external
    created=0
    if ip link show "$tap" >/dev/null 2>&1; then
      verify_tap
    else
      [[ $action == up ]] || die "refresh requires an existing TAP"
      [[ ! -e $state ]] || die "stale ownership marker; operator review required"
      # The endpoint must not retain an old tenant's NAT mapping at reuse.
      flush_endpoint
      ip tuntap add dev "$tap" mode tap
      printf '%s\n' "$iface" > "$state"
      created=1
      ip addr add "$host_addr" dev "$tap"
      # Keep new TAP down until its deny/allow rules have all been installed.
    fi
    apply_rules
    remove_legacy
    if (( created )); then ip link set "$tap" up; fi
    ;;
  flush-conntrack) flush_endpoint ;;
  down)
    # Only a stopped/receipted slot may be brought down. Never use for refresh.
    # An adopted TAP has no marker and must not be deleted by this helper.
    if [[ -f $state ]]; then
      ip link del "$tap"
      remove_legacy
      rm -f "$state"
    elif ip link show "$tap" >/dev/null 2>&1; then
      die "unowned existing TAP; refusing to remove its isolation rules"
    fi
    remove_rules
    flush_endpoint
    ;;
esac
