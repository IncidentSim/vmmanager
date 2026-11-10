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
| Helper | exact verb/argument validation with a recording fake `systemctl` |
| `vmd` | config validation; combined mode wiring over real mTLS with fake seams |

## NOT proven by `make check`

* That a real Firecracker VM boots from a published image, that TAP/NAT work,
  that SSH through `ProxyJump` works, or that `SendCtrlAltDel` ends a real guest.
* `firecracker-vm@.service`, `fc-vm-network.sh` and `cmd/vm-launcher` on a real
  systemd host (only `vm-launcher`'s Go logic and the helper script are unit-tested).
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
