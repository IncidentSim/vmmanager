# Ubuntu + PostgreSQL image builder

Linux x86_64 / KVM image builder. The unprivileged Go client boots a disposable
Ubuntu ext4 disk, installs PostgreSQL **inside the guest** over SSH, requests a
clean guest reboot, and publishes the modified filesystem. The output is a
Firecracker root disk, not an OCI image or a memory snapshot.

## Service deployment contract

An administrator first installs the units and narrow authorization policy in
`deployment/` (see its installation instructions). Normal application commands
below run as **amnezia**, without sudo. The client may start/stop/restart only
`firecracker-builder@main.service`; the unit owns the VMM process and manages
networking through its dependency. Do not run `network.sh` from clients.

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
  the stable workspace `.client.lock` inode until service cleanup finishes.

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
  -output images/ubuntu-postgres.ext4
bash ./run-image.sh images/ubuntu-postgres.ext4
```

The builder copies the base into the workspace. It never changes the base or
an existing output. After a successful guest reboot, it copies the working disk
to a temporary file **in the output directory**, syncs it, and atomically renames
it with no-replace semantics. Workspace and output may be different filesystems.
An existing output, even one created concurrently, is never overwritten.

The shell wrapper makes its own disposable workspace copy. From another terminal:

```bash
ssh -i assets/id_ed25519 -o StrictHostKeyChecking=accept-new root@172.16.0.2
# Commands below run inside the guest:
systemctl status postgresql
sudo -u postgres psql -Atqc 'SHOW server_version;'
reboot
```

Low-level `./image-builder -run-image /var/lib/firecracker-builder/work/NAME.ext4`
boots that writable file directly. Paths outside the workspace, traversal and
symlink components are rejected. Do not pass a valuable source image. The old
`-kernel`, `-tap`, `-firecracker`, and `-console-log` flags have been removed.

## Lifecycle and diagnostics

The client uses `systemctl --no-ask-password start/stop` for the fixed unit and
the official Firecracker SDK **Client** for Unix HTTP API configuration. It
waits for `GET /` InstanceInfo `Not started`, then configures 2 vCPUs, 1024 MiB,
`/kernel/vmlinux`, `/work/<relative-disk>`, and `tap-fcpg`, and sends
`InstanceStart`. It does not use SDK Machine/process spawning.

Guest `reboot=k` makes Firecracker exit. The client polls systemd state and
accepts a retained `active/exited` state (or an inactive state whose metadata is
still available), `Result=success`, `ExecMainStatus=0`, and normal
`ExecMainCode=exited` (numeric 1). `RemainAfterExit=yes` prevents systemd from
garbage-collecting that exit metadata before the client checks it. The client
then stops the unit, releasing networking. A failed/killed service is never success.
On errors or signals the client stops the unit and verifies it is not active
before releasing its lock. If stopping cannot be confirmed, the builder retains
the working disk. Wrappers conservatively retain their copies on any runner
failure; inspect and remove them only after confirming the unit is stopped.
Do not SIGKILL the client: it would bypass its cleanup.

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
permissions, stable locking, systemd outcomes/cleanup/cancellation, official SDK
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
A successful report is written only after both clean boots; no past SDK-spawned
verification result is evidence that a new systemd deployment works.

Guest provisioning currently uses DNS 1.1.1.1. Production images should regenerate
guest identity/SSH host keys and avoid reusable demo credentials.

## Sources

- [Firecracker Getting Started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
- [Firecracker Network Setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md)
- [Firecracker FAQ](https://github.com/firecracker-microvm/firecracker/blob/main/FAQ.md)
