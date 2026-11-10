# Multi-VM deployment (PROPOSED source only)

Nothing here has been installed or run on a host. Every step needs explicit
operator approval. It is **separate** from the active legacy
`firecracker-builder@main.service`, which stays untouched.

| | legacy (active) | multi-VM (proposed) |
|---|---|---|
| VMM unit | `firecracker-builder@main` | `firecracker-vm@N` (one VMM process per slot, `Restart=no`) |
| network | `firecracker-builder-network` | `firecracker-vm-network@N` (host-root oneshot per slot) |
| runtime / jail | `/run/firecracker-builder`, `.../jail` | `/run/firecracker-vms/slot-N`, `/var/lib/firecracker-vms/jail` |
| TAP / subnet | `tap-fcpg`, `172.16.0.0/30` | `tap-vmN`, `PREFIX.N.0/30` from **your** `pool.conf` |
| privileged control | systemd | `fc-vm-helper.socket` -> `start-slot N` only |
| storage | `.../storage` (20 GiB) | **same** mount, `.../storage/vms/{work,images}` |

## Pieces

* `cmd/vm-launcher` -> `/usr/local/libexec/fc-vm-launcher`: per-slot init shim and
  atomic, fsynced, root-owned exit receipt (`<invocation>.exit.json`).
* `firecracker-vm@.service`: jailed VMM. **It contains no networking commands**:
  `RootDirectory=` applies to every `Exec*` line of that unit, so the jail has no
  shell/ip/iptables and none are copied in. It `Requires=`/`After=` the network unit,
  and fails (Assert*, not silently skipped) unless
  `/var/lib/firecracker-builder/storage` is a mount point, the work directory
  exists and `/dev/kvm` exists; `RequiresMountsFor=` adds the mount dependency.
* `firecracker-vm-network@.service` + `fc-vm-network.sh`: host-root oneshot,
  `RemainAfterExit`. Creates `tap-vmN`, the /30 and tagged NAT rules; **refuses any
  existing non-default route or interface address that overlaps the slot /30**
  (a covering `172.17.0.0/16` counts) and any existing interface; removes only what
  its marker recorded. It stays up while the slot is idle and is stopped by you
  (`systemctl stop firecracker-vm-network@N`) or at shutdown, always after the VMM
  unit (and therefore after the receipt).
* `fc-vm-helper.socket` / `fc-vm-helper@.service` -> `cmd/vm-helper` (root) and
  `cmd/vm-helper-client` (what `vmd` runs as `helper`): the privilege boundary.
  The socket is `root:<group> 0660`; anything that can connect can do exactly one
  thing: ask to start an **idle** `firecracker-vm@N.service`. There is no stop, kill,
  restart, path, unit-name or command verb, and no sudoers/polkit rule.
* `pool.conf.example` -> `/etc/firecracker-vms/pool.conf`: the operator-chosen
  guest pool and slot count, read by the network script, the helper and `vmd`
  (`vmd` refuses to start if its config disagrees).
* `vmd.example.json`: facade-only listener, mTLS, local backend with required
  storage mount guard and aggregate admission.

## Choosing the guest pool

Pick `GUEST_PREFIX` yourself (slot N = `PREFIX.N.0/30`, guest `.2`). Do not use
Docker's `172.17/16`, the legacy `172.16.0.0/30`, or anything your LAN/VPN
routes. The shipped `10.213` is a placeholder. The network script re-checks
routes at every `up`.

## Budget (do not read the slice as admission)

Admission is enforced by `vmd`: `max_vms`, `max_total_mem_mib`, `max_total_vcpu`,
and each profile must satisfy `mem_mib + vmm_overhead_mib <= unit_mem_limit_mib`
(= the unit's `MemoryMax`, 768M). The instances run in the **legacy** slice
`system-firecracker\x2dbuilder.slice` (CPUQuota 100%, MemoryMax 2G, see
`deployment/resources.md`), so that 2 GiB / 1 CPU is shared with the legacy VMM,
not multiplied. The slice is a ceiling that kills on overrun; `vmd` is what refuses
excess work earlier. Keep `max_total_mem_mib` plus VMM overhead and any legacy guest
inside 2 GiB; do not run a legacy build concurrently. With one vCPU of quota, two
guests share it.

## Storage

`vmd` requires `storage_mount` and checks, before writing anything, that it is a
mount point and that `work_root` and `images_dir` are on that device. The unit has
the same guard (`AssertPathIsMountPoint`, `RequiresMountsFor`). Both are
fail-closed: an unmounted 20 GiB volume can never fall back to the root filesystem.
Do not repartition or format anything.

## Verification you can run now

`make verify-units` renders the units into a throwaway root and runs the real
`systemd-analyze verify` (syntax and dependency wiring; not installed, not
activated, no privileges). It does **not** prove runtime behaviour of the sandbox.
`make check` runs it, plus unit/contract tests and the network-script collision
tests against canned `ip` output.

## Proposed activation order (operator)

1. Build `vmd`, `vm-launcher`, `vm-helper`, `vm-helper-client` (`make vmd vm-launcher vm-helper vm-helper-client`).
2. Choose and install `/etc/firecracker-vms/pool.conf`.
3. Create jail, `.../storage/vms/{work,images}`; install binaries (root-owned) to
   `/usr/local/libexec/{fc-vm-launcher,fc-vm-network,fc-vm-helper,fc-vm-helper-client}`.
4. Render `@CLIENT_GROUP@` and install the four unit files; `daemon-reload`;
   enable `fc-vm-helper.socket` (not the VM units).
5. PKI outside Git; run `vmd` as the unprivileged user with the helper path
   `/usr/local/libexec/fc-vm-helper-client`.
6. Operator runs `scripts/integration/multi-vm.sh` (see `docs/verification.md`).

## Restart behaviour of vmd

`vmd` takes exclusive locks on its work, run and images directories and holds them
until it exits. Stopping `vmd` never stops a VM. On the next start it reconciles:
a VM whose exit receipt exists becomes exited; one without a receipt keeps its
lease and is marked retained (Stop then sends an identity-checked shutdown for a
known invocation, and refuses for an unknown one). Unreadable workspace metadata
blocks all new admission until you review it.

## Rollback

Stop `vmd`; make sure no slot has a running guest (`systemctl list-units
'firecracker-vm@*'`); stop `firecracker-vm-network@*` and disable the helper socket;
remove the new units, helpers and directories. Legacy service/CLI are unaffected.

## Limits (honest)

* The unit and network script have only been syntax/dependency-verified, not run.
* Operator procedure for a retained VM whose launch outcome is unknown: confirm
  with `systemctl status firecracker-vm@N` that no unit is active, then remove that
  workspace directory by hand; `vmd` will not discard it for you.
* All slots bind the shared `/work`; guest isolation from each other is the
  trusted-operator model, not a boundary.
* Shared preinstalled SSH keys in the base image let anyone with that key log into
  every VM derived from it.
