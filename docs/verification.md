# Verification: what is and is not proven

## Automated (`make check`, no root, no KVM)

`make check` runs `go test -race -cover ./...`, the Python unit tests (legacy
verifier/layout/mTLS and `deployment/multivm/test_helper.py`), `go vet`, `bash -n`
on all scripts, and `make proto-check` (regenerates protobuf with pinned tools and
fails if committed code differs).

| Area | Evidence (all with FAKES or loopback, never a real VM) |
|---|---|
| Facade `vmlib.Service` | external-package tests: create/start, SSH metadata, idempotent/concurrent stop+delete, commit, no overwrite, cap and unique-IP races, cancellation, retained disks, no publish without receipt |
| mTLS | real loopback gRPC with ephemeral PKI: valid; no client cert; client cert from wrong CA; server from wrong CA; hostname mismatch; constructors fail closed |
| Remote backend/facade | error mapping to sentinels, ID/path/key rejection before the backend is called, local / facade-over-remote-backend / combined composition, separate trust domains |
| Local backend | slot-derived resources, admission cap, symlink/oversize/profile rejection, replacement-VMM identity refusal, original-receipt requirement, no-replace publication, release only with proof, restart reconciliation fails closed, corrupt metadata blocks admission - through fake launcher/VMM seams |
| Ownership and recovery | second owner over the same / overlapping / symlinked roots rejected; close and reopen reconcile; restart + Stop/Commit/Delete use identity-checked shutdown; unknown identity fails closed; ambiguous launcher failure keeps lease and slot; stale Release cannot delete a re-created workspace (deterministic hook test) |
| Close lifecycle | a blocking `CopyFile` held across `Close` (second owner refused until drained, late failure cleanup cannot touch the new owner's workspace), a concurrent Close/Prepare/Start/Publish/Release stress with a byte-exact tree fingerprint after Close returns, `WaitExit` released by Close |
| Receipt trust | writable runtime parent/ancestor, untrusted owner (test seam), missing/symlink/world-writable/directory run lock, unsafe existing slot dirs, forged receipts (symlinked slot dir, symlinked receipt, group/world-writable receipt or slot dir, receipt as a directory) refused by WaitExit/Release/Publish; owner/mode rules over synthetic `stat` data (chown is not possible unprivileged) |
| Reservations, publish | profile edits across a restart (memory and vCPU) and legacy/unknown-profile metadata counted conservatively; two publishers of one target with a deterministic hook, marker survives, classification survives reopen |
| Stale reads | a parked `Get` raced against Stop/Commit/Delete and several parked readers, under `-race` |
| Shipped policy | the real `vmd.example.json` through the real parser and backend admits two guests and refuses a third; the offline integration test derives its cap from the same file |
| Data races | concurrent Get/List/SSHInfo against Stop/Commit/Delete with a delayed receipt under `-race` |
| Durability, admission | marker-before-image publication, classification survives restart, aggregate memory/vCPU caps, profile vs unit-limit, storage-mount guard before any write |
| Privilege boundary | one-line protocol validation, pool.conf parsing, idle-unit-only handler (fake `systemctl`), client refusal of anything but `start-slot N` |
| Network script | collision logic against canned `ip` output: covering/more-specific routes, interface addresses, other slots, default route, bad pool/slot input, root requirement |
| Units | static contracts (no network exec in the jail, Requires/After network unit, Assert*/RequiresMountsFor storage guard, socket mode/group) and `make verify-units` (real `systemd-analyze verify` on rendered copies) |
| Integration script | offline sequence/admission test with fake `vmctl`/`ssh`: source is deleted before descendants under `max_vms=2` |
| `vmd` | config validation; combined mode wiring over real mTLS with fake seams |

## NOT proven by `make check`

* That a real Firecracker VM boots from a published image, that TAP/NAT work,
  that SSH through `ProxyJump` works, or that `SendCtrlAltDel` ends a real guest.
* The units, `fc-vm-network.sh` and the helper on a real systemd host.
  `systemd-analyze verify` checks syntax and dependency wiring only; it does not run
  the jail, the `Requires=`/`After=` start/stop order, `AssertPathIsMountPoint`, the
  socket activation or the network script against a real kernel.
* Root ownership of the real runtime tree: the owner checks are exercised only with
  synthetic `stat` data and a test uid seam. systemd's handling of `RuntimeDirectory=`
  sub-directories (modes/owners) and `systemd-tmpfiles` are not exercised.
* That the guest base honours `Guest.Configuration` (MAC-derived address) in practice.
* Real cgroup budget enforcement and the shared 20 GiB storage bound.
* The privilege path by which `vmd` may invoke the helper (operator-defined).
* Multi-tenant isolation (a non-goal) or host-key pinning.

## Operator-run integration (requires approval; not executed by the author)

After the operator installs the proposed units/helper and starts `vmd`:

```sh
FC_INTEGRATION_CONFIRM=yes-run-on-real-host \
FC_ADDR=vmd.example.com:19450 FC_SERVER_NAME=vmd.example.com \
FC_CA=ca.crt FC_CERT=client.crt FC_KEY=client.key \
FC_BASE_IMAGE_ID=ubuntu-base FC_PROFILE=small FC_SSH_KEY=$HOME/.ssh/id_ed25519 \
FC_BASE_IMAGE_PATH=/var/lib/firecracker-vms/images/ubuntu-base.ext4 \
bash scripts/integration/multi-vm.sh
```

It checks: boot -> SSH -> external mutation -> stop -> commit new image ->
overwrite refused -> two independent VMs -> persisted marker in both -> breakage
in one does not affect the other -> distinct addresses -> base hash unchanged.
Record the output with date and host in your own notes; this repository does not
claim it passes.

## Also verify on the host before trusting resource limits

`systemctl show 'system-firecracker\x2dbuilder.slice' -p CPUQuotaPerSecUSec -p MemoryMax`,
`findmnt` for the work/images mounts with the same `st_dev` as the legacy 20 GiB
filesystem, and that `max_vms` x profile memory plus VMM overhead fits the shared
2 GiB budget.
