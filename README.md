# Build an Ubuntu + PostgreSQL Firecracker image with Go

A small **Linux x86_64 / KVM** demo. The Go program boots an Ubuntu microVM via
`firecracker-go-sdk`, executes `apt-get install postgresql` **inside the guest** over SSH,
checks PostgreSQL, cleanly shuts down the guest and publishes a new, bootable
`ext4` root filesystem image.

This produces a **Firecracker disk image**, not an OCI/Docker image, nor a
Firecracker memory snapshot. A Firecracker VM boots with a guest kernel plus
an ext4 root filesystem file.

## Requirements

- x86_64 Linux with working KVM (`ls -l /dev/kvm`)
- Go 1.22+
- root access for this demo's TAP networking and Firecracker
- Internet access from the host and guest
- Roughly 5 GB free disk space

Install host dependencies on Ubuntu/Debian:

```bash
sudo apt-get update
sudo apt-get install -y curl ca-certificates squashfs-tools e2fsprogs \
  openssh-client iproute2 iptables gawk grep coreutils
```

`prepare-ubuntu.sh` follows the **official Firecracker getting-started recipe**
for obtaining a recent CI Ubuntu rootfs and matching guest kernel. Artifacts
are retrieved from Firecracker's own CI S3 bucket, and a demo SSH key is
injected. The script also downloads the latest Firecracker release executable.

## Run

```bash
sudo ask-exec bash ./prepare-ubuntu.sh # Ubuntu rootfs, Firecracker kernel, SSH key
sudo ask-exec bash ./network.sh up           # tap-fcpg: 172.16.0.1/30, NAT to default route

go build -o image-builder .
sudo ask-exec ./image-builder -output images/ubuntu-postgres.ext4

sudo ask-exec bash ./network.sh down         # clean up the TAP and this demo's iptables rules
```

If your outbound network interface differs from the first default-route
interface, override it: `sudo HOST_IFACE=ens5 ./network.sh up` (and use the
same override for `down`). The IP range `172.16.0.0/30` must not conflict with
existing host networks.

Expected stages:

```
[1/5] Copying Ubuntu rootfs
[2/5] Starting Firecracker microVM via Go SDK / Unix API
[3/5] Waiting for SSH in Ubuntu
[4/5] Installing PostgreSQL inside Ubuntu
... apt output ...
[5/5] Rebooting guest to shut down Firecracker cleanly
Built /absolute/path/images/ubuntu-postgres.ext4
```

The `images/ubuntu-postgres.ext4` is the output. The original
`assets/ubuntu-base.ext4` is not modified. `cp --reflink=auto --sparse=always`
tries to use copy-on-write when possible, but works without reflink support.

## Boot the resulting image

Firecracker requires the `assets/vmlinux` kernel as well as the ext4 disk. To
try a new VM from your built image, keep the demo TAP running and launch:

```bash
sudo ask-exec bash ./network.sh up    # skip if already running
sudo ask-exec bash ./run-image.sh images/ubuntu-postgres.ext4
```

In another terminal, after boot, connect using the demo SSH key:

```bash
ssh -i assets/id_ed25519 -o StrictHostKeyChecking=accept-new root@172.16.0.2
systemctl status postgresql
sudo -u postgres psql -Atqc 'SHOW server_version;'
reboot       # terminates the Firecracker process gracefully
```

`run-image.sh` uses a temporary copy so this test does not mutate the built
image. After it exits, shut down network using `sudo ask-exec bash ./network.sh down`.

## Mechanics

1. `prepare-ubuntu.sh` turns Firecracker's Ubuntu squashfs into a bootable
   ext4 disk, including a root SSH public key.
2. `network.sh up` makes a TAP and NAT rules; the guest's Ubuntu network
   configuration assigns `172.16.0.2` from MAC `06:00:AC:10:00:02`.
3. `main.go` copies the base rootfs. `vm.go` constructs a typed SDK config,
   creates a machine with `firecracker.NewMachine`, and calls `Start`.
   The SDK launches the Firecracker executable with `--api-sock`, configures
   the kernel, writable root disk, 2 vCPUs, 1024 MiB RAM, and the existing
   TAP through its Unix HTTP API, then sends `InstanceStart`.
4. Go waits for SSH, installs and enables PostgreSQL through a guest-side
   shell script, then checks `SHOW server_version;`.
5. Go asks the guest to reboot. On x86_64, `reboot=k` makes Firecracker exit
   after a graceful guest reboot. Go waits with SDK `Machine.Wait`.
   Only **after** a successful exit does Go atomically
   rename the modified working disk to the requested output image.

The SDK dependency is pinned to `github.com/firecracker-microvm/firecracker-go-sdk`
v1.0.0 in `go.mod`/`go.sum`. The Firecracker executable and Linux/KVM are still
required: the SDK is an API client and process orchestrator, not a replacement
hypervisor. Network setup and SSH provisioning are deliberately unchanged.

`run-image.sh` and `verify-image.py` use the same SDK backend through
`image-builder -run-image PATH`. This low-level flag boots **the supplied writable
file directly**; use the shell wrapper for an automatic disposable copy.
Optional `-console-log PATH` captures guest console output. Interrupting a VM
aborts it; it is not a substitute for clean guest reboot when saving a disk.

## Tests

```bash
go test -race -cover ./...
go vet ./...
python3 -m unittest -v test_verify_image.py
```

Unit tests check the SDK configuration, API socket command, startup validation,
runner CLI, and that both image helpers use the SDK runner. They do not require
root or KVM. The Python regression test checks verifier cleanup on SIGTERM
without starting a VM or changing networking. The two-boot SQL verifier below
is the real KVM integration test.

## Limitations

This is a learning example, **not a hardened production image pipeline**.
It assumes one builder at a time, static TAP/MAC/IP, demo SSH keys and a
trusted local guest network. The SSH host keys and machine identity are baked
into the output image and should be regenerated for production clones. For
multi-tenant work use Firecracker's jailer, resource limits, per-VM networking,
image verification, reproducible/pinned dependencies, and cleanup of guest
identities/credentials. Do not run `network.sh up` repeatedly without cleaning
up the original TAP. `network.sh down` intentionally leaves the host's global
`net.ipv4.ip_forward` value unchanged.

The guest uses 1.1.1.1 for DNS during provisioning. Change this in `main.go`
if your network requires another resolver.

SDK runtime verification completed with Firecracker v1.18.0, guest kernel 6.18.56+,
Ubuntu 24.04.5 LTS, and PostgreSQL 16.15. The built image was booted using a
disposable copy, a SQL row was inserted, and the row survived a complete VM
restart. The original output image remained unchanged; offline `e2fsck -fn`
passed.

To repeat the two-boot SQL test (requires the demo TAP to be absent initially):

```bash
go build -o image-builder .
sudo ask-exec env TMPDIR="$TMPDIR" python3 ./verify-image.py
```

The verifier creates and removes the demo TAP/NAT, waits for PostgreSQL readiness
independently of SSH readiness, and writes `verification.json`.

## Sources

- [Firecracker Getting Started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
- [Firecracker Network Setup](https://github.com/firecracker-microvm/firecracker/blob/main/docs/network-setup.md)
- [Firecracker FAQ — reboot / power-off semantics](https://github.com/firecracker-microvm/firecracker/blob/main/FAQ.md)
