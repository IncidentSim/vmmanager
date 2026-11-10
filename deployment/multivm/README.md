# Multi-VM deployment (PROPOSED source only)

Nothing here has been installed or run on a host. Every step below needs
explicit operator approval. This deployment is **separate** from the active
legacy `firecracker-builder@main.service`, which must stay untouched.

| | legacy (active) | multi-VM (proposed) |
|---|---|---|
| unit | `firecracker-builder@main` | `firecracker-vm@N` (one VMM process per slot) |
| runtime | `/run/firecracker-builder` | `/run/firecracker-vms/slot-N` |
| jail | `/var/lib/firecracker-builder/jail` | `/var/lib/firecracker-vms/jail` |
| TAP / subnet | `tap-fcpg`, `172.16.0.0/30` | `tap-vmN`, `172.17.N.0/30` (guest `.2`) |
| restart | `Restart=always` | `Restart=no` (idle until started) |
| privileged control | systemd | `fc-vm-helper start-slot N` only |

## Pieces

* `cmd/vm-launcher` -> `/usr/local/libexec/fc-vm-launcher`: per-slot init shim and
  atomic root-owned exit receipt (`<invocation>.exit.json`).
* `firecracker-vm@.service`: the template. Instances run in the legacy
  `system-firecracker\x2dbuilder.slice`, so the **aggregate 2 GiB / 1 CPU budget
  is shared**, not multiplied. Legacy guest and multi-VM guests together must fit
  it: with the default profile (512 MiB) use `max_vms` of 2 or less, and do not run
  a legacy build at the same time.
* `fc-vm-network.sh` -> `/usr/local/libexec/fc-vm-network`: per-slot TAP/NAT; refuses
  existing interfaces/routes; removes only what its marker recorded.
* `fc-vm-helper.sh` -> root-owned helper with exactly one verb. There is no stop,
  kill or restart verb: the guest shuts down gracefully and the library waits for
  the receipt. **How vmd (unprivileged) is allowed to invoke it is your decision**
  (e.g. one exact-command rule); `vmd.example.json` points at a placeholder
  `helper` path you provide. No sudo is embedded anywhere in this repository.
* `vmd.example.json`: facade-only listener, mTLS, local backend. Serve the raw
  backend only when you need it (`serve_backend`), on its own CA/cert/key.

Keep `MAX_SLOTS` in both scripts equal to `max_vms` (<= 64; the budget makes 2-4 realistic).

## Storage

Put `/var/lib/firecracker-vms/work` (and the images directory) on the **same
bounded 20 GiB filesystem** as the legacy work/jobs trees, as a further bind-mounted
subdirectory. Verify with `findmnt`/`df` that the `st_dev` matches and is not `/`.
Do not repartition or format anything for this.

## Proposed activation order (operator)

1. Build `cmd/vm-launcher` (static) and `cmd/vmd`; review the unit and scripts.
2. Create jail/work/images directories with the owners the unit expects.
3. Install launcher, network script, helper and the unit with root ownership.
4. Generate PKI outside Git (`deployment/generate-mtls.sh`), one CA per listener.
5. `systemctl daemon-reload`; start `vmd` as the unprivileged user.
6. Run `scripts/integration/multi-vm.sh` (see `docs/verification.md`).

## Rollback

Stop `vmd`; make sure no slot has a running guest (`systemctl list-units
'firecracker-vm@*'`), confirm no retained workspaces you still need, then remove
the new units, helpers and directories. The legacy service/CLI are unaffected.

## Limits (honest)

* All slots bind the shared `/work`, so one VMM *could* name another VM's disk
  path; isolation between guests is the trusted-operator model, not a boundary.
* Shared preinstalled SSH keys in the base image mean anyone with that key can log
  into every VM derived from it.
* Guest host keys are not pre-learned; clients use per-VM known-hosts files.
