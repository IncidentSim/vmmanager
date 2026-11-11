# Architecture: vmlib

Decisions and rationale: [ADR 0001](adr/0001-vm-library-and-remote-backends.md).
Plan: [`plans/vm-library.md`](plans/vm-library.md).

## Packages

| Package | Role | Needs KVM? |
|---|---|---|
| `vmlib` | public types, `Backend` interface, `Lifecycle` interface, `Service` facade | no |
| `vmlib/vmlibtest` | fake in-memory backend for consumers' tests | no |
| `vmlib/local` | local host backend (slots, TAP/IP/MAC, workspace disks, receipts, publication) | yes, at runtime |
| `internal/localcfg` | the single private JSON policy for the local backend (validation + assembly into `local.Config`) | assembly is linux-only |
| `cmd/vmapi` | HTTP control API; embeds `vmlib.Service` over `vmlib/local.Backend` in-process, owns DBOS | host |
| `cmd/vmctl` | HTTP API CLI client built on `controlclient` (JSON, `ssh`/Ansible output, JWT bearer from a private file/env) | no |
| `cmd/vm-launcher` | per-slot launcher + receipt writer (proposed, runs in jail) | host |
| `cmd/vm-helper`, `cmd/vm-helper-client` | root side / unprivileged side of the one-request `start-slot N` boundary (proposed) | host |
| `cmd/vmbot`, `internal/vmbot` | Telegram multi-VM bot (up to `max_vms`, per-VM capability-link ttyd terminals), an HTTP API client ([ADR 0002](adr/0002-telegram-single-vm-bot.md), [ADR 0003](adr/0003-vmbot-multi-vm.md), [`vmbot.md`](vmbot.md)) | no |
| `internal/vmhelper` | protocol, strict `pool.conf` parsing, idle-unit-only handler | no |
| `examples/pgprovision` | PostgreSQL provisioning as an application over `Lifecycle` | no |

The legacy CLI (`package main`) and `internal/remote` HTTPS build manager are unchanged.

## Using the library from another module

```go
import (
    "github.com/petruekhin/vibe-firecracker/vmlib"
    "github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// Service over any vmlib.Backend. On the host, vmapi uses vmlib/local.Backend
// (opened from the private backend policy by internal/localcfg); consumers'
// tests use the in-memory fake:
svc, _ := vmlib.NewService(vmlibtest.New(2, "ubuntu-base"))
var l vmlib.Lifecycle = svc

vm, _ := l.Create(ctx, vmlib.CreateRequest{ImageID: "ubuntu-base", Profile: "small"})
info, _ := l.SSHInfo(ctx, vm.ID)          // host/port/user + bastion for ssh -J
_, _ = l.Stop(ctx, vm.ID)                  // graceful, idempotent
img, _ := l.Commit(ctx, vm.ID, "golden")  // NEW image; never overwrites
_, _ = l.Delete(ctx, vm.ID)                // frees disk only when exit is proven
```

Remote consumers use the HTTP API through `controlclient` (or `vmctl`), not a VM
RPC. The portable public SDK packages are `vmlib`, `vmlib/vmlibtest` and
`controlclient`; `scripts/consumer-smoke.sh` builds them from an external module
and cross-builds them (and `vmctl`) for macOS/Windows.

## Semantics worth knowing

* A call's `context` bounds that call only. Cancelling a `Create` RPC after the VM
  started does not stop it; use `Stop`/`Delete`. The library installs no signal
  handlers (`vmapi` and `vmctl` own them).
* `Stop`/`Delete` are idempotent and serialized per VM. `Commit` requires the
  original VMM's exit receipt, publishes with no-replace semantics and never opens
  the base image writable.
* Anything uncertain (shutdown, identity, configuration, restart) sets
  `retained` on the VM and keeps the lease; `Delete` then returns
  `vmlib.ErrRetained`. Check `VM.Retained` / `RetainedReason`.
* A local backend owns its roots exclusively until `Close()`. `Close` stops admitting
  operations (`local.ErrClosed`), waits for the admitted ones, then unlocks; it never
  stops VMs, so it can block as long as an in-flight operation (cancel its context to
  shorten it).
  A second backend over the same (or overlapping/symlinked) roots fails at `New`.
* Over the HTTP API the VM ID is server-assigned and mutations are idempotent by
  `Idempotency-Key`; at the `vmlib` level a `Create` takes a caller ID, so after a failed
  or lost call `Get`/`Delete` that ID. `VM.Retained` is a flag over the lifecycle state.
* `WaitSSHReady` uses a prober. The default dials TCP, which only works where the
  host-private guest IP is routable (on the host). A remote client should pass
  `vmlib.WithSSHProber` that goes through its bastion (e.g. `ssh -J ... true`).

## Deployment shape (`vmapi -config FILE`)

One process: `vmapi` opens `vmlib/local.Backend` directly from its `backend`
object (exact former `vmd` `backend.local` fields; mapping in
[`control-api-backend.md`](control-api-backend.md)), wraps it in `vmlib.Service`
and serves the authenticated HTTP API, Swagger and the terminal gateway. There is
no VM RPC listener, no backend/facade mTLS and no `vmd`. A config containing the
removed `facade` section is rejected. The backend holds exclusive lifetime
flocks on its roots, so an old `vmd` must be stopped first.

## VM owner workflow

Design: [RFC](rfc-vm-owner.md). Code: `internal/vmbot/owner.go` (owner, effect
child, projection) and `internal/vmbot/control.go` (admission, handoff, recovery).

* **One owner per VM.** `vmbot-owner-<vmID>` (workflow `vmbot-vm-owner`) is created
  with the VM and stays `PENDING` between commands; it ends `SUCCESS` only after
  confirmed deletion. It runs directly, never on a queue slot. Two VMs' owners
  are independent.
* **Commands.** Admission (`AcceptControl`) atomically reserves identities,
  quota, names and image references in the store, then `dbos.Send`s
  `OwnerCmd{OpID, Kind, Digest}` with the operation ID as idempotency key
  (Create instead starts the owner). Send success is durable delivery only.
  The owner re-checks kind/digest against the stored request, so message
  deduplication can never hide a changed payload; the changed-payload check for
  an `Idempotency-Key` itself is the store digest, evaluated before any effect.
* **Sequential.** The owner runs `begin` (validate, mark `running`, set the
  transitional phase) -> one finite child `vmbot-vm-effect` (workflow ID
  `<owner>.<opID>`) -> `project` (write the outcome), then `Recv`s the next
  command. The child performs the backend effect inside one step and returns a
  typed `EffectOut`; it never writes application state. Delete waits behind any
  running/earlier command. There is no preemption, priority mailbox,
  admission-time revocation or external fence.
* **Failures.** Expected child failures (backend errors, even a panicking child)
  are recorded as operation outcomes (`failed` + code) and the owner continues.
  An unexpected owner failure ends it `ERROR`: it stays visible (`owner_error`,
  failed pending operations, recovery error/degraded health) and the VM and its
  capacity stay retained. No replacement owner is started automatically.
* **Authoritative writers.** Admission writes only *new* records: VM identity
  (phase `creating`), operations (`pending`), private image reservations
  (`publishing`) and the image `deleting` fence for shared-image deletion. After
  that the owner's `begin`/`project` steps are the only writers of VM `phase`,
  `state`, resources, VM removal, operation `running`/`succeeded`/`failed` and
  image `ready`. The shared-image deletion workflow (`vmbot-image-delete`) writes
  its own operation/image outcome. Store writes in these steps are idempotent
  and safe to replay.
* **Local projections.** Terminal processes/links are rebuildable and not
  checkpointed: the owner rebuilds a running VM's terminal once after a process
  start (only for VMs present at start) and the terminal command/revocation run
  as effects. SSH metadata/readiness is a read (`GET .../ssh`), not an owner command.
* **Replay compatibility.** Pinned: workflow names `vmbot-vm-owner`,
  `vmbot-vm-effect`, `vmbot-image-delete`, DBOS application version
  `vmbot-owner-1`, step names `begin`/`effect`/`project`, topic `cmd`, IDs above
  (asserted in `owner_version_test.go`). The application version is constant so
  living owners are recovered after a rebuild, which means **a constant version
  does not make a code change compatible**: any change to the order or number of
  durable calls in `vmOwner`/`ownerHandle` must be guarded with `dbos.Patch`
  (insert new durable calls only behind `Patch`; never reorder or remove them
  without a drain plan), or the old history fails with an unexpected-step error
  and the owner ends `ERROR`. The owner's `Recv` timeout loop adds one history
  row per idle period (6 h); there is no history rollover in this iteration.
* **Pre-owner state.** Documented in
  [control-api.md](control-api.md#migration-from-the-legacy-bot-and-handoff):
  settled VMs are adopted without Create; transitional legacy VMs are rejected at
  startup. Legacy workflows (version `vmbot-orch-1`) stay untouched in the
  database. Rollback to an earlier build after owners exist would run a second
  executor and is unsupported.

## Backend contract and trust

See ADR sections 3 and 7. In short: opaque validated IDs, server-owned roots and
profiles, no paths/commands/addresses from callers, trusted-operator
(not multi-tenant) authorization; user isolation is done by the HTTP API (JWT
username binding, per-user ownership). See [ADR 0007](adr/0007-remove-grpc-vm-plane.md).

## Migration and rollback

The legacy CLI, `firecracker-builder@main.service`, HTTPS build manager and
`deployment/remote.md` procedures are unchanged. The VM-plane migration is not
additive: the old `vmd` and new API cannot own the same roots concurrently. Stop
the bot/API and `vmd`, back up API JSON + SQLite + WAL together while stopped,
and move the exact old `.backend.local` object into the API's top-level `backend`.
Preserve existing images, metadata, receipts, VM IDs and delivery offsets; do not
recreate or clean the backend roots. Keep old binary/config/PKI for rollback.
Rollback after new writes requires current-state compatibility review, never a
stale state restore or blanket removal of installed helpers/networking. See
[the mapping and handoff](control-api-backend.md) and
[deployment instructions](../deployment/multivm/README.md).

## No generated code

The protobuf/gRPC definitions, generated Go code, generator tooling and
`make proto`/`proto-check` were removed. `make no-grpc-check` fails if any
application package imports gRPC/protobuf (it also reports, without failing,
whether a third-party dependency still pulls them in transitively).
