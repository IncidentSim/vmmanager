# Root-owned Firecracker service, unprivileged client

## One-time install

Prepare `assets/firecracker`, `assets/vmlinux`, the Ubuntu base image and demo SSH
key first. The VMM and launcher must be static Linux executables. This deployment
expects IPv4 forwarding to already be enabled and refuses an existing `tap-fcpg`
or route for `172.16.0.0/30`; it does not replace unrelated networking.

```bash
CGO_ENABLED=0 go build -o "$TMPDIR/firecracker-builder-launcher" ./cmd/service-launcher
sudo ask-exec bash deployment/install.sh "$(id -un)" "$TMPDIR/firecracker-builder-launcher"
```

The installer copies the VMM and kernel to root-owned `/opt/firecracker-builder`,
installs the system units, static launcher and a narrow polkit rule. It does not
enable a VM at boot or add the client to sudo/systemd administrative groups.
No general-purpose privileged command runner is installed.

## Granted access

- Only the selected user may start/stop/restart `firecracker-builder@main.service`
  without authentication. Other services, unit-file edits and daemon-reload are
  not granted.
- Firecracker runs as **root** with the client's primary group, in a filesystem
  jail rooted at `/var/lib/firecracker-builder/jail`.
- API socket: `/run/firecracker-builder/main.sock`, root/client-group, mode 0770.
  Its containing directory is root-owned 0750; the client cannot replace the
  socket. Unix socket permissions use read/write bits; its executable bits do
  not grant execution of a program.
- Host workspace: `/var/lib/firecracker-builder/work`; the VMM sees `/work`.
  The kernel is visible as `/kernel/vmlinux`. Host home directories and `/etc`
  are not exposed in the jail. Executables and kernel are read-only bind mounts.
- The root VMM has only `CAP_NET_ADMIN`, not `CAP_SYS_ADMIN`, `CAP_SYS_PTRACE` or
  `CAP_DAC_OVERRIDE`. Disk files must be group-readable/writable (0660); temporary
  workspace directories must be group-traversable (0770).
- A systemd dependency creates the fixed TAP and narrowly scoped, commented
  iptables rules. After the client verifies the VMM exit and stops its retained
  unit, `StopWhenUnneeded` removes the network. It may take
  a brief moment after `systemctl stop` for dependency cleanup to finish.

The namespace options are defense in depth, not a promise to hide all host
metadata: on this systemd setup `/proc` and `/sys` metadata remained visible
despite masking directives. Negative API probes confirmed that the host's
`/etc/shadow`, `/proc/1/root/etc/shadow`, and writable access to the mounted
kernel were denied. Do not equate those probes with a complete sandbox audit.

This is a trusted local development service, not a multi-tenant security
boundary. The API controls a root process with KVM and host-network privileges.
Do not expose the socket over TCP or grant access to untrusted users. All members
of the selected primary group have socket access; choose a private group/user.
One VM at a time is supported; use only the `@main` instance.

## Normal operation — no sudo

```bash
systemctl --no-ask-password start firecracker-builder@main.service
systemctl show firecracker-builder@main.service -p MainPID -p ActiveState -p Result
curl --unix-socket /run/firecracker-builder/main.sock http://localhost/
systemctl --no-ask-password stop firecracker-builder@main.service
```

The application configures the VM using the official Go SDK client. systemd owns
process startup, termination, journaling and resource limits. A guest reboot
exits the VMM; `Restart=no` is intentional. `RemainAfterExit=yes` retains its
exit metadata until the client has checked it and stopped the unit. A stopped, killed or failed VMM is
not proof of a clean disk shutdown: the builder must validate the systemd exit
status before publishing an image. See the project README for build and SQL
verification commands.

## Rollback

```bash
sudo ask-exec bash deployment/uninstall.sh
```

This revokes the polkit permission, stops only these services and removes only
this installation's unit files/helpers/immutable assets. It deliberately retains
`/var/lib/firecracker-builder` and the source project's assets/images. Review
and delete retained disks separately if no longer needed.
