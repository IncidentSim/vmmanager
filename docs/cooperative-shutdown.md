# Optional authenticated cooperative shutdown

`personal_ssh.shutdown_known_hosts_file` is **disabled when omitted or empty**.
It requires `personal_ssh.enabled=true` and an existing operator-trusted
known_hosts file. Example (paths only, no secrets):

```json
"personal_ssh": {
  "enabled": true,
  "store_dir": "/var/lib/vmapi/credentials",
  "master_key_file": "/var/lib/vmapi/master.key",
  "shutdown_known_hosts_file": "/etc/vmapi/guest_known_hosts"
}
```

Do not enable until host-key policy is established out of band. Startup rejects
relative/unclean paths, SSH expansion tokens, whitespace, missing/empty/malformed
files, symlinks, group/world-writable files, untrusted owners or replaceable
ancestry. Use root/service-owned directories and file mode 0644 or stricter.
The file must authenticate the **direct guest IP** derived from the backend pool;
this host-local operation never uses the advertised bastion. Do not copy terminal
TOFU files blindly, auto-accept keys, or use `/dev/null` as the trust file.

The local backend optional interface is:

```go
type CooperativeShutdown interface {
    Reboot(ctx context.Context, vmID, host, user string) error
}
// local.Config.CooperativeShutdown; nil preserves the original path.
```

Only VMs persisted as `AccessManaged` use it. After the usual original-invocation
check and CAD request (including an ambiguous CAD error), the backend checks the
original receipt/identity again before invoking the hook. It checks again after
SSH, including an ambiguous acknowledgement. Identity mismatch is never ignored.

The implementation `sshaccess.SSHShutdown` leases **only the dedicated per-VM
terminal key**, under `<store_dir>/shutdown-leases`, and invokes `/usr/bin/ssh`
without a local shell. It ignores user/system SSH config, disables agent/shared
identity/password/interactive auth and multiplexing, and requires strict host-key
checking against the explicit file, with global known_hosts disabled. There is no
key discovery or host-key update. The remote command is exactly `systemctl reboot`;
no force, sudo, signal or kill fallback is attempted.

Each attempt has a five-second context deadline (or the earlier caller deadline),
a two-second connect timeout and one connection attempt. No stdout/stderr is
captured or logged; errors are generic. The local SSH process is waited before
its private-key file lease is removed. Timeout may terminate that **local SSH
client**, never the VMM or guest. SSH readiness/auth/host-key failure is best-effort:
with identity still confirmed, existing shutdown receipt waits/retries continue.
With the hook enabled an ambiguous CAD acknowledgement also does not end retries.
Disabled/legacy CAD behavior is unchanged.

Neither CAD nor SSH success, EOF, socket disappearance or timeout is exit proof.
The original invocation's trusted exit receipt is required before releasing a
started VM's resources. The raw backend accepts a valid receipt proving process
exit even when it records an unclean exit; this hook does not strengthen that
existing contract. Stop/Commit additionally require a clean receipt, whereas
Delete may discard an already-exited VM after an unclean exit. Uncertainty about
process exit retains the disk and slot. A missing system bus, denied reboot
privilege, absent SSH binary, stale host keys or an unready guest may therefore
still leave a retained VM. Nothing in this change promises recovery without
operator-established trust and a functioning guest shutdown path.

This change does not deploy, restart services, set a host-key policy, or alter the
controller's logging sinks. Existing controller logging needs a separate audit
before enabling it globally, because it includes existing raw error paths.
