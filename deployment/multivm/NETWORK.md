# Slot networking: public SSH and default isolation (source only)

Nothing here has been activated by the implementation/tests. All commands below
are **operator-only host-root changes requiring a separately approved scope**.
Never run them from the API worker, helper socket protocol, or inside a VMM jail.

## Configuration contract

`/etc/firecracker-vms/pool.conf` remains root:root 0644, with a root-owned,
non-client-writable parent. Literal `KEY=VALUE`, no surrounding whitespace,
inline comments, interpolation, quotes or duplicate/unknown keys. Empty lines,
column-zero `#` comments and CRLF are accepted. The Go network tool and helper share the same parser and canonical value rules:

- `GUEST_PREFIX=10.213`, required, existing two-octet pool contract.
- `MAX_SLOTS=4`, required, 1..64 (four-slot source proposal uses slots 0..3).
- `MAX_SLOT_CPU_MILLI=500`, optional, existing whole-percent CPU ceiling.
- `PUBLIC_IPV4`, optional canonical dotted IPv4, not private/special-use. It must
  be assigned to the single default-route interface at activation. No hostname,
  IPv6, leading zero, upstream/provider-only NAT address or auto-discovery.
- `SSH_PORT_BASE`, optional canonical decimal 1024..65535, with
  `base + MAX_SLOTS - 1 <= 65535`. Public keys must be specified together; both
  absent means publication **disabled**. Example `22000` reserves 22000..22003
  for a four-slot pool. No per-request/per-VM override exists.

For slot N, only TCP arriving on the default external interface for
`PUBLIC_IPV4:(SSH_PORT_BASE+N)` is DNATed to `10.213.N.2:22`. No OUTPUT DNAT,
hairpin publication, UDP, extra guest ports or source SNAT is installed. External
client source is retained; conntrack reverses the DNAT on the normal return path.
Guest routing must use its `.1` host gateway, DNS should use public `1.1.1.1`
(TCP and UDP allowed), and guest sshd/key policy is a separate prerequisite.
The public host/base advertised by the unprivileged application is a **separate
localcfg configuration**, not a new guest/client input; align it operationally
with this root pool. The network helper never edits that config.

## Isolation and ownership

- Explicit `iptables-nft`, `ip6tables-nft` and their `*-restore --noflush` tools;
  no switch of firewall backend, global flush/policy change, UFW or Docker edit.
- Slot-owned chains `FCVM<N>F`, `I`, `N`, `M`, `V6` and hook comment
  `firecracker-vms:slot<N>` are reserved to this tool. Do not create unrelated
  rules in these chains. Dedicated chains are rebuilt atomically **per table**;
  there is no all-tables transaction. Initial hook installation is incremental.
- Guest IPv4 source must equal the slot's `.2`. Guest-initiated NEW host INPUT
  is denied to **every** local address, including the public address. There are
  no default host-management exceptions. Replies to host-initiated connections
  are allowed, so existing host-to-guest diagnostics remain possible.
- Cross-slot, RFC1918, CGNAT, loopback, link-local/metadata, multicast/reserved
  ranges are denied before ordinary established handling. Global-address routes
  on non-uplink devices (including VPN/policy tables) are snapshotted and denied.
  Non-uplink forwarding is denied regardless. Public egress through the selected
  external interface and its established replies are allowed; guest egress
  MASQUERADE remains. Only exact DNAT SSH REPLY traffic can bypass destination
  blocks (e.g. an external client with a private source on the public uplink).
- IPv6 INPUT/OUTPUT/FORWARD on the TAP is denied, without changing host-wide
  IPv6. There is intentionally no IPv6 opt-out knob in this version.
- Review policy routing, local L2 bridges, other nft base-chain priorities and
  competing firewall managers before approval. Existing TAP must be a TAP with
  the expected `.1/30`; this tool does not enslave/reconfigure it. Unrelated
  firewall managers can still block permitted traffic or reorder hooks. Refresh
  after route/uplink changes, and recheck hook precedence after firewall reloads.
  A new earlier foreign ACCEPT can bypass these hooks until refresh. Refresh
  atomically repositions the exact owned hook ahead of such rules, preserving foreign rules;
  do not allow concurrent firewall rewrites during activation, and audit readback.

## Safe install / refresh without restarting active VMs

First record the old installed tool/unit/pool and full IPv4/IPv6 firewall
snapshots **for review**, not for a blind global restore. Inspect listeners and
other NAT mappings to reserve the entire port range (DNAT has no host listener).
Verify the old marker `/run/firecracker-vms-net/N.iface` is root-owned if present.
Provision the `conntrack` userspace tool and both nft iptables variants separately
if missing. Verify `net.ipv4.ip_forward=1`; the tool never changes this sysctl.
The latest helper binary/parser must be installed before enabling new root pool
keys; the old helper rejects unknown keys. That binary install is separate from
this source-only network change. Preserve all existing pool/CPU/capacity values.

Example root command payloads, from the reviewed repository directory:

```sh
make build
make install CLIENT_GROUP=vmusers # renders all units; does not activate them
# Install the separately reviewed, host-specific pool.conf; do not blindly copy an example.
systemctl daemon-reload
/usr/local/libexec/fc-vm-network check 0
/usr/local/libexec/fc-vm-network refresh 0
/usr/local/libexec/fc-vm-network refresh 1
/usr/local/libexec/fc-vm-network refresh 2
/usr/local/libexec/fc-vm-network refresh 3
```

`refresh` requires an already existing TAP and **does not** add/delete it, set its
link state/address, restart any service/VMM, or flush conntrack. It also works
when the network unit is not active. `up` on an existing valid TAP does the same
safe apply. For an existing active network unit with the new unit definition,
`systemctl reload firecracker-vm-network@N.service` runs this refresh. **Do not
restart/stop the network unit to pick up rules**: ExecStop is destructive for
owned TAPs, and stopping a required unit can stop a dependent VMM.

`up N` creates a missing TAP down, flushes the targeted public tuple first,
installs rules, then brings it up. On partial failure it deliberately retains the
TAP/marker and any already installed isolation rather than deleting a potentially
live interface. Reapply after fixing the cause; if a failed newly created TAP is
still down, only the operator may bring it up after verifying all rules.
Existing old single-interface markers are honored. The exact three old tagged
FORWARD/MASQUERADE rules are removed only after the new rules have been applied.
Adopted TAPs without old ownership markers are never claimed or deleted;
`down` refuses to strip their isolation while the interface exists.

Read back each exact slot using `iptables-nft -S FCVM0F`,
`iptables-nft -S FCVM0I`, `iptables-nft -t nat -S FCVM0N`,
`iptables-nft -t nat -S FCVM0M`, `ip6tables-nft -S FCVM0V6`, plus the INPUT,
FORWARD, PREROUTING and POSTROUTING hooks. Compare foreign rules with the preflight
snapshot. From a **different external host**, verify the public SSH banner/key
and actual guest identity for each occupied slot, reject adjacent ports/wrong
interface, and confirm guest DNS/internet plus blocked host/private/cross-slot,
source-spoof and IPv6 paths. Host-local public-IP probes do not exercise DNAT.
Mock tests do not establish kernel behavior or public reachability.

## Slot reuse, disabling publication, and rollback

These are persistent per-slot rules, not a per-VM lifecycle controller. Neither
network unit state nor firewall existence is an authoritative VM death receipt.
The allocator/operator must retain the slot until its existing death receipt
and credential cleanup permit reuse. With the updated root `fc-vm-helper`
binary, **every public-enabled `start-slot`**, including tenant reuse of a
retained TAP, executes this fixed internal command:

```sh
/usr/local/libexec/fc-vm-network flush-conntrack N
```

The helper holds its existing per-slot flock, confirms systemd state is
`inactive` or `failed`, flushes, then sets CPUQuota, verifies quota readback, and
starts the unit. Active/transitioning or busy slots never flush; a flush failure
fails closed before quota mutation or start. The socket request still has only
`start-slot N CPU_MILLI`: no caller-selected executable, verb, or publication
flag. Publication is derived only from the parsed root pool; disabled pools
retain the old no-flush path. This is per-start enforcement, not network-unit
`ExecStart`: that unit's `RemainAfterExit=yes` would skip cleanup on TAP reuse.
The network global lock nests inside the helper slot lock; the network tool
never takes a helper slot lock, so there is no reverse lock order.

Deploy the updated helper binary **and** `fc-vm-helper@.service` before enabling
automatic public reuse. Before socket activation provision the existing runtime
paths root-owned, including `/run/firecracker-vms-net` with mode 0755; the
helper's strict filesystem sandbox grants write access only to that path and
`/run/firecracker-vms`. Missing paths are not silently ignored, nor is `/run`
made writable. Preserve the network state directory across accepted helper
connections (do not use per-connection `RuntimeDirectory` cleanup). The service
allows `AF_NETLINK` alongside `AF_UNIX` because conntrack uses kernel netlink;
no IPv4/IPv6 socket families are enabled. Install `conntrack` separately if
missing. These are deployment prerequisites, not startup changes performed by
unprivileged application code.

This deletes only IPv4 TCP conntrack entries for the original public destination
and slot port, including their NAT reply state. Repeated deletion of zero entries
is accepted only with conntrack's explicit zero-deletions diagnostic; errors fail
closed. `refresh` never runs this command automatically. Direct operator starts
that bypass the helper must still explicitly flush the slot after confirmed
death/credential cleanup and before starting the next guest. If changing public
IP/base, delete old tuples using the old pool first; after config changes the
helper only knows the new tuple. Existing guest-initiated egress conntrack is not
globally flushed by this helper.

To roll back **publication while retaining active guest networking/isolation**:
remove only `PUBLIC_IPV4`/`SSH_PORT_BASE` from the reviewed pool, refresh occupied
slots, then delete old public tuples explicitly using the recorded old values:

```sh
/usr/local/libexec/fc-vm-network refresh 0
# Replace OLD_PUBLIC_IPV4 and OLD_SLOT_PORT with recorded literals, separately reviewed.
conntrack -D -f ipv4 -p tcp --orig-dst OLD_PUBLIC_IPV4 --dport OLD_SLOT_PORT
```

A refresh with publication disabled removes DNAT/new inbound permission without
tearing down the TAP or egress. Existing conntrack sessions can persist until
explicit deletion. Keep this hardened tool when rolling back public access;
restoring the old broad-ACCEPT tool would undo isolation. If a full network
teardown is required, only after separately verifying the VMM is dead/receipted,
run `down N` (or stop an idle owned network unit). It deletes only a marked TAP
and the exact slot hooks/chains. Never restore full host firewall snapshots
blindly: that would overwrite unrelated concurrent changes.

## Source verification

```sh
go test -race ./internal/vmnet ./internal/hostinstall ./internal/vmhelper
make verify-units
```

Go tests exercise the network runner with canned command output and validate
repeated apply, foreign rules, TAP ownership, tuple binding, IPv6 denial,
anti-spoof/private/host policy and targeted conntrack cleanup. Installer tests
render units and check runtime trust. These are root-free checks, not real
kernel/firewall evidence; use the opt-in Go E2E and external-host probes for that.
