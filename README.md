# Ubuntu + PostgreSQL image builder

The current transport is a local Unix socket only. A future remote controller
over HTTPS/mTLS is deferred; it will also need server-side disk operations and
guest access routing. No remote API listener is installed by this version.

Linux x86_64 / KVM image builder. The unprivileged Go client boots a disposable
Ubuntu ext4 disk, installs PostgreSQL **inside the guest** over SSH, requests a
clean guest reboot, and publishes the modified filesystem. The output is a
Firecracker root disk, not an OCI image or a memory snapshot.

## Service deployment contract

An administrator installs, enables and starts the units externally using
`deployment/` (see its installation instructions). Normal application commands
below run as **amnezia**, without sudo. The service runs permanently in the
background, with `Restart=always`, `RestartSec=1`, `RemainAfterExit=no` and
`RuntimeDirectoryPreserve=restart`. The client only connects to an existing API
socket: it never starts, stops, restarts or inspects host services. Networking
is a deployment concern. Do not run `network.sh` from clients.

- Host API socket: `/run/firecracker-builder/main.sock`, root:amnezia and group
  writable. This socket is privileged: only trusted local clients may access it.
- Stable shared workspace: `/var/lib/firecracker-builder/work`, amnezia:amnezia,
  mode 0770. The jail sees it as `/work`.
- Fixed host kernel: `/opt/firecracker-builder/vmlinux`; the jail sees
  `/kernel/vmlinux`. The client neither checks nor opens a local kernel.
- Working directories use mode 0770; disks use 0660. The root-owned VMM runs
  with group amnezia and without DAC override, so these group permissions are
  necessary. Source images and SSH keys need only be readable by the client.
- One VM/client at a time: every client holds a nonblocking exclusive flock on
  the stable workspace `.client.lock` inode throughout API preflight, disk copy,
  guest execution, receipt verification and publication/cleanup.

The root-owned filesystem jail and capability policy are deployment concerns.
The application never launches Firecracker, uses sudo, or configures host
networking. This remains a single-tenant example with a trusted guest network,
static TAP/MAC/IP and demo SSH credentials, not a multi-tenant build platform.

## Build and use

Requirements: installed deployment, Go 1.22+, Python 3, OpenSSH client, coreutils,
working host KVM, guest Internet access, and sufficient disk space. Prepare a
matching Ubuntu base rootfs and SSH key in `assets/` using the administrator's
artifact preparation workflow (`prepare-ubuntu.sh`); ensure these resulting
files are accessible to amnezia. Install the matching kernel in the fixed
service location during deployment.

```bash
go build -o image-builder .
./image-builder -base assets/ubuntu-base.ext4 -ssh-key assets/id_ed25519 \
  -socket /run/firecracker-builder/main.sock -output images/ubuntu-postgres.ext4
bash ./run-image.sh images/ubuntu-postgres.ext4
```

The builder first checks the existing API with a 2-second timeout and requires
`Not started` plus a valid per-process invocation ID, before copying the base or
creating output directories. An offline/busy server fails immediately; start or
restart it externally as an operator. `-socket` defaults to
`/run/firecracker-builder/main.sock`; the receipt directory is its parent.
The builder never changes the base or an existing output. After a successful
guest reboot and a clean exit receipt for that exact VMM, it copies the working disk
to a temporary file **in the output directory**, syncs it, and atomically renames
it with no-replace semantics. Workspace and output may be different filesystems.
An existing output, even one created concurrently, is never overwritten.

The shell wrapper makes its own disposable workspace copy. From another terminal:

```bash
ssh -i assets/id_ed25519 -o StrictHostKeyChecking=accept-new root@172.16.0.2
# Commands below run inside the guest:
runuser -u postgres -- pg_isready
runuser -u postgres -- psql -Atqc 'SHOW server_version;'
reboot
```

Low-level `./image-builder -run-image /var/lib/firecracker-builder/work/NAME.ext4`
boots that writable file directly. Paths outside the workspace, traversal and
symlink components are rejected. Do not pass a valuable source image. The old
`-kernel`, `-tap`, `-firecracker`, and `-console-log` flags have been removed.

## Lifecycle and diagnostics

The client uses the official Firecracker SDK **Client** for Unix HTTP API
configuration. A fail-fast `GET /` requires InstanceInfo `Not started` and a
32-character lowercase hexadecimal ID supplied by the launcher from
`$INVOCATION_ID`. It configures 2 vCPUs, 1024 MiB,
`/kernel/vmlinux`, `/work/<relative-disk>`, and `tap-fcpg`, and sends
`InstanceStart`. It does not use SDK Machine/process spawning.

Guest `reboot=k` makes Firecracker exit. The launcher atomically records its
exit in `<socket-parent>/<invocation-id>.exit.json`, root:amnezia mode 0640
inside a root:amnezia mode 0750 directory. Only a receipt matching the original
ID, with `result="success"`, `exit_code="exited"`, and `exit_status="0"`, proves a
clean shutdown. Missing, malformed, mismatched, failed or killed-process
receipts never permit publication or deletion. A new process on the same
socket is **not** evidence of a clean old exit. Receipts survive automatic
restart; explicit administrator stops may remove the runtime directory.

The unit remains active globally and automatically launches a fresh VMM after
each guest exits. Applications never stop it or tear down networking. On errors
or signals after `InstanceStart`, cleanup checks process identity and requests
`SendCtrlAltDel` through the SDK (the available x86 guest reboot action), then
waits for the original exit receipt within a bounded 30-second cleanup window.
It never reboots a replacement VMM. Normal provisioning still uses SSH guest
`reboot`, not the cleanup API action.

Before guest start, partial configuration has no safe API process cleanup.
If a clean process exit cannot be proved, the working disk is retained and the
error tells the operator to restart the VMM externally. Wrappers conservatively
retain their copies on any runner failure, including cancellation even when the
guest subsequently exits cleanly. Inspect retained disks and remove them only
after an operator confirms no VMM can still access them. Do not SIGKILL the
client: it would bypass its cleanup. Unavailable deployment/receipt support is
an explicit error, never simulated success.

VMM and guest console output belong to the root-owned service journal, not the
client's stdout. Read it with the administrator's journal access policy:

```bash
journalctl -u firecracker-builder@main.service
systemctl --no-ask-password show \
  --property=ActiveState,SubState,Result,ExecMainStatus,ExecMainCode \
  firecracker-builder@main.service
```

## Verification

```bash
go test -race -cover ./...
go vet ./...
python3 -m unittest -v test_verify_image.py
bash -n run-image.sh demo-lifecycle.sh
```

Unit tests need neither root nor KVM: they check jail path validation, file
permissions, stable locking, per-process receipts/cleanup/cancellation, official SDK
requests against a fake Unix HTTP server, output preservation, and the CLI.
The Python subprocess regression sends real SIGTERM and verifies runner cleanup.

Real KVM integration, after deployment and image creation:

```bash
python3 ./verify-image.py
# Optional:
FC_VERIFY_IMAGE=images/ubuntu-postgres.ext4 \
FC_VERIFY_REPORT=verification.json python3 ./verify-image.py
bash ./demo-lifecycle.sh
```

The verifier runs unprivileged, copies the image into the shared workspace,
boots it twice through the same runner, performs SQL INSERT/SELECT persistence
checks, records the **actual SQL server version**, and verifies the published
image hash did not change. It never changes networking or file ownership.
Its `boot-*.log` files capture runner diagnostics, not guest console output.
Both helpers use `FC_SOCKET` (default `/run/firecracker-builder/main.sock`);
`verify-image.py` also accepts `-socket`. They perform a read-only API preflight
before copying. The two-boot verifier waits for external automatic restart
between successful receipt-proven guest exits, records the number of clean exits, and
checks the published image hash; it never asserts that the service stops.
A successful report is written only after both clean boots; no past SDK-spawned
verification result is evidence that a new systemd deployment works.

Guest provisioning currently uses DNS 1.1.1.1. Production images should regenerate
guest identity/SSH host keys and avoid reusable demo credentials.

## Sources

- [Firecracker Getting Started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
- [Firecracker Network Setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md)
- [Firecracker FAQ](https://github.com/firecracker-microvm/firecracker/blob/main/FAQ.md)
