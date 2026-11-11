# Build, install and uninstall

Everything host-side is done by small Go commands; the Makefile only
orchestrates. See [ADR 0009](adr/0009-go-tooling-install-ci.md) for the contract.

## Make targets

| Target | What it does | Privilege |
|---|---|---|
| `make build` (default) | Static (`CGO_ENABLED=0`) product executables into `bin/`: `vmapi vmbot vmctl vm-launcher vm-network vm-helper vm-helper-client vmhost` | none |
| `make legacy` | Legacy single-VM builder tools (`image-builder`, `remote-client`, `service-launcher`). Not part of the product; no installer or unit deploys them | none |
| `make test` | `go test -race ./...` including the Go tooling tests | none |
| `make check` | `vet`, `gofmt`, tests, build, `stage-check`, `verify-units` | none |
| `make control-smoke` | Executable API smoke: the real `vmapi` entry point with an in-memory backend over loopback. **Not** an E2E | none |
| `make stage-check` | Install into a temporary root, verify exact modes, uninstall, require no files left | none |
| `make verify-units` | Render the units and run the real `systemd-analyze verify` (`VERIFY_FLAGS=--require-systemd-analyze` fails if it is absent) | none |
| `make install` | Place files under `DESTDIR`/`PREFIX` (files only) | root unless `DESTDIR` |
| `make uninstall` | Remove exactly the files recorded in the install manifest | root unless `DESTDIR` |
| `make preflight` | Read-only host report: KVM, `/dev/net/tun`, systemd, tools, forwarding, `pool.conf`, subnet collisions, storage mount, assets | none |
| `make host-setup` | Create *empty* jail/work/images directories (`USER_NAME=vmapiuser`, add `APPLY=1` to act; otherwise prints the plan) | root |
| `make host-enable` | `daemon-reload` + enable the helper socket only (`APPLY=1` to act) | root |
| `make e2e-live` | Opt-in E2E against an already deployed vmapi (`VMAPI_E2E=1`, [e2e-live.md](e2e-live.md)) | none |
| `make e2e-kvm` | Real E2E on a disposable KVM host ([ci.md](ci.md)) | needs a provisioned host |
| `make release-check` | Validate `.goreleaser.yaml` and build a snapshot into `dist/`; never publishes | none |

Variables: `PREFIX` (default `/usr/local`), `DESTDIR`, `SYSCONFDIR` (`/etc`),
`CLIENT_GROUP` (primary group of the unprivileged vmapi user; defaults to the
group of `SUDO_USER` or the current user), `ASSET_DIR`, `STORAGE_ROOT`,
`FIRECRACKER`/`KERNEL` (optional files to install as assets), `FORCE=1`.

## What install places

Nothing is started, enabled, reloaded, formatted or configured.

| Path | Mode | Content |
|---|---|---|
| `PREFIX/bin/{vmapi,vmbot,vmctl,vmhost}` | 0755 | user-facing executables |
| `PREFIX/libexec/{fc-vm-launcher,fc-vm-network,fc-vm-helper,fc-vm-helper-client}` | 0755 | root helper, per-slot launcher (must be static: it runs in the VMM jail), network tool, helper client |
| `SYSCONFDIR/systemd/system/{firecracker-vm@.service,firecracker-vm-network@.service,fc-vm-helper@.service,fc-vm-helper.socket}` | 0644 | units rendered against the actual paths and client group |
| `SYSCONFDIR/tmpfiles.d/firecracker-vms.conf` | 0644 | root-owned runtime tree + the client-writable lock file |
| `PREFIX/lib/systemd/user/{vmapi,vmbot}.service` | 0644 | user units (`ExecStart` points at `PREFIX/bin`); never enabled |
| `PREFIX/share/doc/vm-firecracker/examples/**` | 0644 | `pool.conf`, vmapi/vmbot JSON, Nexterm, idle-expiry, four-slot capacity examples; documentation only |
| `ASSET_DIR/{firecracker,vmlinux}` | 0755/0644 | installed/replaced when `FIRECRACKER=`/`KERNEL=` are given; retained on upgrades that omit them |
| `PREFIX/share/vm-firecracker/install-manifest.json` | 0644 | ownership record (path, mode, SHA-256) |

Defaults for the storage volume and asset directory are the compatibility paths
existing hosts already use (`/var/lib/firecracker-builder/storage`,
`/opt/firecracker-builder`); override with `STORAGE_ROOT`/`ASSET_DIR`.

Safety rules enforced by `vmhost` (and tested): destinations must stay inside
`DESTDIR`; a symlinked ancestor or target is refused; paths are restricted to
`[A-Za-z0-9_./+-]` because they are embedded in units; unmanaged files that differ are
never overwritten without `FORCE=1` (nothing is written if any conflict exists);
writes are atomic. `pool.conf`, keys, images, VM state and firewall rules are never
created, changed or removed by install/uninstall. Uninstall removes only manifest
files whose content is unchanged (modified ones are kept and reported; `FORCE=1`
removes them) and directories the install created if they are empty. It does not stop
units: do `systemctl disable --now fc-vm-helper.socket` and stop slot units first.

## Staged install (no root)

```sh
make build
make install DESTDIR="$PWD/stage" CLIENT_GROUP="$(id -gn)"
make uninstall DESTDIR="$PWD/stage" CLIENT_GROUP="$(id -gn)"
```

## Real installation (explicit, as root)

```sh
make build                      # as your user
sudo make install CLIENT_GROUP=vmusers            # files only
sudo bin/vmhost preflight --group vmusers         # read-only report with remedies
# Create/choose pool.conf yourself (root:root 0644); see the installed examples:
#   /etc/firecracker-vms/pool.conf   (GUEST_PREFIX must overlap nothing on the host)
sudo make host-setup USER_NAME=vmapiuser APPLY=1  # empty dirs + systemd-tmpfiles
sudo make host-enable APPLY=1                     # helper socket only
```

Host prerequisites the tools check but never change: KVM and TUN, systemd as PID 1,
`ip`, `iptables-nft` and `ip6tables-nft` (+ `-restore`), `conntrack`, IPv4 forwarding
enabled, a storage volume **already mounted** at the storage root (units fail closed
on the root filesystem), and the Firecracker/kernel assets. `host-setup` creates only
missing directories and never changes existing ones. Slot networking (TAP, NAT,
firewall chains) is created only when the helper starts a slot, by
`firecracker-vm-network@N.service`; see `deployment/multivm/NETWORK.md`.

Then configure vmapi/vmbot from the installed examples, enable the user units as
the vmapi user, and follow the handoff steps in [control-api.md](control-api.md).

## Release archives

Tagged releases ship `vm-firecracker_<version>_linux_<arch>.tar.gz` (all product
executables plus `vmhost`, flat) and portable `vmctl_*` client archives, with
`checksums.txt` (SHA-256). The extracted directory *is* the installer's build
directory:

```sh
sha256sum -c checksums.txt --ignore-missing
tar xzf vm-firecracker_<version>_linux_amd64.tar.gz && cd vm-firecracker_<version>_linux_amd64
sudo ./vmhost install --group vmusers
```

Release packaging is configured in `.goreleaser.yaml` (GoReleaser v2) and run by
`.github/workflows/release.yml` on `v*` tags only. Locally: `make release-check`.

## Documented exceptions to "no shell"

`internal/sshaccess/supported/fcnet-setup.sh` is guest boot content embedded into
provisioned images and runs inside the microVM; it stays a shell template. No host-side `.sh` or Python script is required; Make recipes and CI steps still
use the platform shell for thin command orchestration.

## Removed legacy capability

The single-VM `firecracker-builder@main.service` deployment (its installer,
network unit, mTLS generator and runtime helpers) is no longer shipped or
installed from this repository. Existing hosts keep running it; see ADR 0009.
