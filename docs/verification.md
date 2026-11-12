# Verification: what is and is not proven

## Automated (`make check`, no root, no KVM)

`make check` runs Go race/coverage tests, vet, format checking, product build,
exact staged install/uninstall and rendered-unit verification. Go tooling tests
cover host networking, installation, no-RPC dependencies and external consumers.
No Python or host Bash script is required.

| Area | Evidence (all with FAKES or loopback, never a real VM) |
|---|---|
| Facade `vmlib.Service` | external-package tests: create/start, SSH metadata, idempotent/concurrent stop+delete, commit, no overwrite, cap and unique-IP races, cancellation, retained disks, no publish without receipt |
| HTTP API auth and client | real HTTP handler + `controlclient`: bearer/JWT negatives, username binding, loopback-only plaintext, `vmctl` token-file permission/argv/leak checks and non-loopback plaintext refusal (the removed gRPC mTLS negatives have no VM transport to apply to; builder HTTP/mTLS tests are unchanged) |
| Embedded local backend | `vmlib.Service` directly over the real local backend (fake launcher/VMM only), dynamic resources, image-reference guards |
| Local backend | slot-derived resources, admission cap, symlink/oversize/profile rejection, replacement-VMM identity refusal, original-receipt requirement, no-replace publication, release only with proof, restart reconciliation fails closed, corrupt metadata blocks admission - through fake launcher/VMM seams |
| Ownership and recovery | second owner over the same / overlapping / symlinked roots rejected; close and reopen reconcile; restart + Stop/Commit/Delete use identity-checked shutdown; unknown identity fails closed; ambiguous launcher failure keeps lease and slot; stale Release cannot delete a re-created workspace (deterministic hook test) |
| Close lifecycle | a blocking `CopyFile` held across `Close` (second owner refused until drained, late failure cleanup cannot touch the new owner's workspace), a concurrent Close/Prepare/Start/Publish/Release stress with a byte-exact tree fingerprint after Close returns, `WaitExit` released by Close |
| Receipt trust | writable runtime parent/ancestor, untrusted owner (test seam), missing/symlink/world-writable/directory run lock, unsafe existing slot dirs, forged receipts (symlinked slot dir, symlinked receipt, group/world-writable receipt or slot dir, receipt as a directory) refused by WaitExit/Release/Publish; owner/mode rules over synthetic `stat` data (chown is not possible unprivileged) |
| Reservations, publish | profile edits across a restart (memory and vCPU) and legacy/unknown-profile metadata counted conservatively; two publishers of one target with a deterministic hook, marker survives, classification survives reopen |
| Stale reads | a parked `Get` raced against Stop/Commit/Delete and several parked readers, under `-race` |
| Shipped policy | the real `deployment/vmapi/backend.local.example.json` through the real `internal/localcfg` parser and backend admits two guests and refuses a third; the offline integration test derives its cap from the same file |
| Data races | concurrent Get/List/SSHInfo against Stop/Commit/Delete with a delayed receipt under `-race` |
| Durability, admission | marker-before-image publication, classification survives restart, aggregate memory/vCPU caps, profile vs unit-limit, storage-mount guard before any write |
| Privilege boundary | one-line protocol validation, pool.conf parsing, idle-unit-only handler (fake `systemctl`), client refusal of anything but `start-slot N` |
| Go network tool | collision logic against canned `ip` output: covering/more-specific routes, interface addresses, other slots, default route, bad pool/slot input, root requirement |
| Units | static contracts (no network exec in the jail, Requires/After network unit, Assert*/RequiresMountsFor storage guard, socket mode/group) and `make verify-units` (real `systemd-analyze verify` on rendered copies) |
| Go E2E harness | root-free fixture/admission tracking and cleanup tests; real guest lifecycle requires explicit live opt-in |
| `vmapi` backend config | strict `backend` loading, stale `facade` rejection, `localcfg` validation and assembly of every policy field; in-process start/stop/restart with lifetime release counted |

## NOT proven by `make check`

* That a real Firecracker VM boots from a published image, that TAP/NAT work,
  that SSH through `ProxyJump` works, or that `SendCtrlAltDel` ends a real guest.
* The units, the Go network executable and the helper on a real systemd host.
  `systemd-analyze verify` checks syntax and dependency wiring only; it does not run
  the jail, the `Requires=`/`After=` start/stop order, `AssertPathIsMountPoint`, the
  socket activation or the network script against a real kernel.
* Root ownership of the real runtime tree: the owner checks are exercised only with
  synthetic `stat` data and a test uid seam. systemd's handling of `RuntimeDirectory=`
  sub-directories (modes/owners) and `systemd-tmpfiles` are not exercised.
* That the guest base honours `Guest.Configuration` (MAC-derived address) in practice.
* Real cgroup budget enforcement and the shared 20 GiB storage bound.
* The privilege path by which `vmapi` may invoke the helper (operator-defined).
* Multi-tenant isolation (a non-goal) or host-key pinning.

## Operator-run integration (requires approval)

Use the Go suite with the explicit settings in [e2e-live.md](e2e-live.md), or
provision a disposable host using [CI/KVM instructions](ci.md).

```sh
VMAPI_E2E=1 make e2e-live # requires the documented VMAPI_E2E_* configuration
```

It checks: boot -> SSH -> external mutation -> stop -> commit new image ->
overwrite refused -> two independent VMs -> persisted marker in both -> breakage
in one does not affect the other -> distinct addresses -> base hash unchanged.
The same lifecycle scenario passed on the historical gRPC reference deployment;
that is not a live test of this rewritten HTTP harness or embedded backend. See
[live verification report](verification-live.md) for the historical configuration,
evidence and deployment-discovered fixes. Every changed deployment needs its own live run. Two-descendant tests require
`max_vms_per_user >= 2` and two free global/backend slots for its principal; do not
raise production policy or remove another user's VM just to run it. With the
one-VM-per-user policy, use the sequential migration acceptance described in
[deployment/multivm/README.md](../deployment/multivm/README.md).

## Also verify on the host before trusting resource limits

`systemctl show firecracker-vms.slice -p CPUQuotaPerSecUSec -p MemoryMax`,
`findmnt` for the work/images mounts with the same `st_dev` as the legacy 20 GiB
filesystem, and that `max_vms` x profile memory plus VMM overhead fits the shared
2 GiB budget.
