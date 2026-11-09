# Externally managed Firecracker service

## Install and start outside the application

Prepare the base image, SSH key, static Firecracker executable and matching kernel
in `assets/` first. The host must have KVM/TUN and IPv4 forwarding enabled. The
network helper refuses an unrelated existing `tap-fcpg` or `172.16.0.0/30` route.

```bash
CGO_ENABLED=0 go build -o "$TMPDIR/firecracker-builder-launcher" ./cmd/service-launcher
sudo ask-exec bash deployment/install.sh "$(id -un)" "$TMPDIR/firecracker-builder-launcher"
```

The administrator-run installer installs root-owned binaries/units and runs
`systemctl enable --now firecracker-builder@main.service`. The application never
runs that command, starts a host process, or starts/stops a service. An earlier
polkit rule allowing client lifecycle operations is removed during upgrade.
Stop the old service externally before upgrading an active installation.

## Always-on process, one guest at a time

- Firecracker runs as root, with the selected client's primary group, in the
  filesystem jail `/var/lib/firecracker-builder/jail`.
- It listens on `/run/firecracker-builder/main.sock` (root/client-group, 0770),
  inside a root-owned 0750 directory. The client cannot replace the socket.
- `Restart=always` and `RestartSec=1` create a fresh VMM after guest reboot/exit.
  One process hosts only one guest. The API is briefly unavailable during restart.
- A dependency maintains TAP/NAT while the service is running, including idle
  time. Stopping the root service externally releases the dependency and network.
- The workspace `/var/lib/firecracker-builder/work` is visible as `/work`; the
  immutable kernel is `/kernel/vmlinux`. Disks need 0660, subdirectories 0770.
- Host home directories and `/etc` are not mounted into the jail. Executables and
  the kernel are read-only. Only CAP_NET_ADMIN is retained; CAP_DAC_OVERRIDE,
  CAP_SYS_ADMIN and CAP_SYS_PTRACE are not granted.

Public `/proc` and `/sys` metadata was still visible on this systemd setup despite
masking directives. Negative API probes denied host `/etc/shadow`,
`/proc/1/root/etc/shadow`, and writable access to the kernel. This is defense in
depth for trusted local development, not a complete sandbox audit or a multi-tenant
boundary. Socket access remains privileged; do not expose it to untrusted users.
All members of the chosen primary group have socket access.

## Exit receipts

The launcher passes systemd's 32-hex INVOCATION_ID as Firecracker's instance ID.
`ExecStopPost` writes an atomic, immutable `<id>.exit.json` beside the socket,
owned by root and group-readable (0640). A successful receipt contains
`result=success`, `exit_code=exited`, `exit_status="0"` and the matching ID.
`RuntimeDirectoryPreserve=restart` keeps receipts across automatic restarts.

Clients use this evidence before publishing or deleting a working disk. A new
responding process alone does not prove a clean exit of the previous one.
This receipt protocol is part of this deployment, not a native Firecracker API.
An arbitrary standalone Firecracker socket without these receipts cannot provide
the same completion guarantee to this example client.

## Client use (no root)

```bash
./image-builder -socket /run/firecracker-builder/main.sock -output images/new.ext4
FC_SOCKET=/run/firecracker-builder/main.sock bash run-image.sh images/new.ext4
```

If the socket is unavailable or the VMM is busy, the client errors before copying
the base disk or creating the output directory. For guest cleanup it can request
x86 SendCtrlAltDel through the SDK; it never restarts the host process. Ambiguous
configuration/cleanup failures retain the disk and require an operator's external
restart. Only a normal guest reboot produces a publishable image.

## Operator commands

These are external administrative operations, never application actions:

```bash
systemctl status firecracker-builder@main.service
curl --unix-socket /run/firecracker-builder/main.sock http://localhost/
sudo ask-exec systemctl restart firecracker-builder@main.service
sudo ask-exec systemctl stop firecracker-builder@main.service
```

The service starts at boot. Idle Firecracker has no guest vCPU/RAM allocation,
but the process, API socket and TAP/NAT remain present. One VM at a time is
supported; only use `@main`.

## Rollback

```bash
sudo ask-exec bash deployment/uninstall.sh
```

This disables/stops the service and removes only this installation's units,
helpers and immutable assets. Workspace images and source-project assets remain
under `/var/lib/firecracker-builder` and the project directory for explicit review.
