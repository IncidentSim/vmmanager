# Public IPv4 SSH endpoint metadata

This is operator policy, not a forwarding API. When `PoolConf` is supplied, the
root-owned pool is authoritative: paired `PUBLIC_IPV4` and `SSH_PORT_BASE` enable
advertisement; omitting both disables it. Absent/null backend `public_ssh` inherits
that root policy. An explicit backend object must match both root values exactly;
it cannot enable publication when the root pool disables it. For example, an
optional redundant backend assertion (not an API Create field) is:

```json
"public_ssh": {
  "host": "203.0.113.10",
  "port_base": 22000
}
```

These are documentation-only values; the parent/operator chooses the audited
actual public IPv4 and unoccupied port range. With a root pool, `host` must be
the exact canonical literal `PUBLIC_IPV4`; DNS names are rejected without any
DNS lookup or startup network dependency. Only library deployments **without
PoolConf** retain the legacy literal IPv4/conservative DNS-name policy; the
operator remains responsible for those names' DNS and publication. IPv6, URLs,
`user@host`, host:port, options, whitespace and malformed DNS names are rejected.
`port_base` must be positive, and `port_base + max_vms - 1 <= 65535`. Reconciliation
also validates the range for existing persisted slots, even after lowering the
admission limit. Configuration is copied at backend construction and is immutable
for that backend lifetime. Returned endpoint metadata does not alias policy.

## Network worker seam

For the four-slot deployment, configure the network worker's operator-only
`PUBLIC_IPV4` and `SSH_PORT_BASE` to the audited actual IPv4 and TCP port base.
Prefer omitting backend `public_ssh` to inherit the root pool. If it is supplied,
set `public_ssh.host` to the exact literal IPv4 and `public_ssh.port_base` to the
identical `SSH_PORT_BASE`:

| Persisted slot | Public destination | Private destination |
| --- | --- | --- |
| 0 | PUBLIC_IPV4:SSH_PORT_BASE | GUEST_PREFIX.0.2:22 |
| 1 | PUBLIC_IPV4:(SSH_PORT_BASE+1) | GUEST_PREFIX.1.2:22 |
| 2 | PUBLIC_IPV4:(SSH_PORT_BASE+2) | GUEST_PREFIX.2.2:22 |
| 3 | PUBLIC_IPV4:(SSH_PORT_BASE+3) | GUEST_PREFIX.3.2:22 |

The backend reads the existing strictly parsed root pool keys; it does not
install firewall rules, change DNS or authorize ingress. Read-only reconciliation
happens before credential-store opening or backend locks/writes, and again at
backend construction. A root change between assembly and opening rejects a stale
resolved policy. The backend uses one pool snapshot for publication and profile
CPU ceilings. A running backend does not watch the pool: root/network publication
changes require coordinated backend restart/reassembly; stale explicit assertions
fail startup, while omitted assertions inherit the new values.
DNAT, guest isolation, outbound NAT, provider/host firewall and independent
off-host login verification are separate deployment work. A green Go test is
not evidence of live ingress. This implementation does not touch host services,
root configuration, credentials or network rules.

The mapping uses each VM's existing persisted slot; there is no new allocator or
lifecycle owner. Stop/restart retains the mapping. Delete and later slot reuse
can assign the same public port to a different VM: verify host keys per VM; do
not treat public port as permanent VM identity.

## Supported Ubuntu guest egress provisioning

The examined `ubuntu-base.ext4` configures a MAC-derived `/30` address using
`/usr/local/bin/fcnet-setup.sh`, but supplies no default route and an empty regular
`/etc/resolv.conf`. Host NAT cannot compensate for the missing guest route.
Operator policy can enable automatic provisioning for **new private copies**:

```json
"guest_network_provisioning": "ubuntu-fcnet",
"guest_dns": ["1.1.1.1"]
```

This option is independent of public SSH advertisement and defaults disabled.
Without it, `guest_dns` remains metadata only, preserving legacy deployments.
This is **not arbitrary Ubuntu support**. Supported images must identify as
Ubuntu in `/usr/lib/os-release`, have a regular `/etc/resolv.conf`, and match the
pinned operational contract copied read-only with `debugfs` from the examined
`/var/lib/firecracker-builder/storage/vms/images/ubuntu-base.ext4`:

- Exact `/usr/local/bin/fcnet-setup.sh` content in
  `internal/sshaccess/supported/fcnet-setup.sh` (executable).
- Exact `/etc/systemd/system/fcnet.service` content in
  `internal/sshaccess/supported/fcnet.service`. It includes the real udev trigger
  and active `ExecStart`, not a comment containing its text.
- Only final LF bytes are ignored for these two comparisons; arbitrary shell
  bodies, commented commands, extra commands and reset `ExecStart` are refused.
- The sysinit wants link must point to that unit; missing/masked links fail.
- Alternate unit files and operational unit-specific/global service drop-ins in
  supported systemd load paths are refused, including reset/override drop-ins.
  Only inert blank/comment-only unrelated drop-ins and the exact generated
  managed `ExecStartPost` are accepted. Unsupported overrides are **not deleted
  or rewritten**; provisioning fails before any disk write. Symlink ancestry is
  refused (the known merged-usr `/lib -> usr/lib` alias is handled explicitly).

Normalized (trailing LF removed) SHA-256 baselines:

```text
2cda553b90c7ac46c0f3b9e701af4dde75a9c322b1679c2680886bc1e6826be5  fcnet-setup.sh
1b515370554f4e1a4dc9bbe83d919068b9bd663535738d3699ade8264afc4e8f  fcnet.service
```

Changing a template is an explicit new-image support/security review, not a
substring compatibility extension. The image and its distro binaries must still
be trusted; this contract is not a general offline guest integrity verifier.
Symlink-managed resolvers and unsupported images fail instead of being rewritten.

The unprivileged offline provisioner uses `debugfs` on the newly copied ext4
workspace, before SSH key installation and first boot. It adds only:

- `/etc/vibe-fc-network.sh`
- `/etc/systemd/system/fcnet.service.d/90-vibe-network.conf`

The drop-in executes the script after the image's existing address setup. It
adds `default via GUEST_PREFIX.slot.1 dev eth0` only when no IPv4 default route
exists, and appends configured nameservers only when the existing resolver has
no nameserver entry. Existing resolver bytes, image setup script and unrelated
comment-only systemd drop-ins are preserved. Managed files are regenerated for
each new copy's slot, including copies of committed derivative images. Conflicting
unmanaged files or symlink targets are refused. An install failure discards the
failed private workspace. No existing VM or base image is rewritten, and no
extra lifecycle writer is introduced.

Regression tests build actual disposable ext4 images, read back files, execute
the generated shell in a sandbox with a fake `ip` command, preserve configured
routes/resolvers, check SSH-key coexistence and run `e2fsck -fn`. An opt-in check
also provisions and fscks a disposable copy of the actual supported base:

```sh
VIBE_NETWORK_BASE_TEST=/path/to/ubuntu-base.ext4 GOMAXPROCS=2 \
  go test -race -p 1 -timeout 180s ./internal/sshaccess -run TestGuestNetwork
```

Mutation regressions reject comment-only setup, altered address masks, appended
shell, commented/missing/reset ExecStart, disabled/wrong enable links,
nonexecutable setup and reset/override/vendor/global drop-ins; disk SHA-256 stays
unchanged on each rejection. The real-base copy test checks source/copy setup,
unit, release and resolver bytes remain unchanged.

These fixes change vmapi composition/provisioning only: rebuild vmapi and record a
new candidate SHA-256 before rollout. Already staged vmapi artifacts are stale;
root-helper source/artifacts and live activation are unaffected by this fix.

This proves offline boot configuration, not a real guest boot. Parent/operator
must still boot a new fixture with the option enabled and verify DNS plus HTTPS
from inside it, then perform the separate off-host public SSH check.

## Consumers and readiness

`vmlib.Service.SSHInfo` and `GET /v1/vms/{id}/ssh` return the configured public
`host` and `port_base + slot`, with no `bastion`. `controlclient.SSH` and `vmctl`
already carry/use the advertised port; use `ssh -p PORT USER@HOST`, not `ssh -J`.
Keep strict per-VM host-key checking, including `[HOST]:PORT` known_hosts names
when PORT is not 22. The E2E pinned SSH helper preserves this port and trust policy.

`HostVM.GuestIP` and guest network metadata remain private. The in-process
`SSHInfo.InternalTarget` is explicitly backend-derived `privateIP:22`, excluded
from JSON. The host web terminal uses that target without a bastion and keeps
its fixed SSH `-p 22`; cooperative shutdown still uses unmodified `GuestIP`.
No client can supply a forwarding host, port or internal target.

API `ready` is a bounded **host-local private TCP observation**, not an ingress,
DNS, provider-firewall, SSH protocol, authentication or user-key validity test.
The API keeps ownership/active-state checks before and after its observation.
It never probes the server's own public forwarded endpoint. The service's
default TCP readiness prober also selects the private target. Older lifecycle
adapters without separate internal metadata retain their existing target.
Custom readiness probers remain responsible for their own semantics.

An SDK caller can obtain the advertised endpoint with
`endpoint, err := client.SSH(ctx, userID, vmID)` and connect from the caller's
network using `endpoint.Host`, `endpoint.Port` and `endpoint.User`. Even if
`endpoint.Ready` is true, independently verify off-host ingress and authenticate
with that VM's key. No SDK request/schema changes are required.
