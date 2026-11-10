# Architecture: vmlib

Decisions and rationale: [ADR 0001](adr/0001-vm-library-and-remote-backends.md).
Plan: [`plans/vm-library.md`](plans/vm-library.md).

## Packages

| Package | Role | Needs KVM? |
|---|---|---|
| `vmlib` | public types, `Backend` interface, `Lifecycle` interface, `Service` facade | no |
| `vmlib/vmlibtest` | fake in-memory backend for consumers' tests | no |
| `vmlib/local` | local host backend (slots, TAP/IP/MAC, workspace disks, receipts, publication) | yes, at runtime |
| `vmlib/remote` | gRPC servers/clients for backend and facade, mTLS helpers | no |
| `vmlib/remote/pkitest` | ephemeral PKI for tests only | no |
| `vmlib/gen/...` | generated protobuf/gRPC (do not edit; `make proto`) | no |
| `cmd/vmd` | daemon: backend-only, facade-only or combined | host |
| `cmd/vmctl` | facade CLI client (JSON, `ssh`/Ansible output) | no |
| `cmd/vm-launcher` | per-slot launcher + receipt writer (proposed, runs in jail) | host |
| `cmd/vm-helper`, `cmd/vm-helper-client` | root side / unprivileged side of the one-request `start-slot N` boundary (proposed) | host |
| `internal/vmhelper` | protocol, strict `pool.conf` parsing, idle-unit-only handler | no |
| `examples/pgprovision` | PostgreSQL provisioning as an application over `Lifecycle` | no |

The legacy CLI (`package main`) and `internal/remote` HTTPS build manager are unchanged.

## Using the library from another module

```go
import (
    "github.com/petruekhin/vibe-firecracker/vmlib"
    "github.com/petruekhin/vibe-firecracker/vmlib/remote"
)

// A) Facade over a REMOTE host backend (client-side orchestration):
tlsCfg, _ := remote.ClientTLSFromFiles(ca, cert, key, "backend.example.com")
be, _ := remote.DialBackend("backend.example.com:19444", tlsCfg)
svc, _ := vmlib.NewService(be)

// B) Portable client of a REMOTE facade (no KVM, no SDK):
var l vmlib.Lifecycle
l, _ = remote.DialFacade("vmd.example.com:19450", tlsCfg) // *FacadeClient

// C) Facade over the LOCAL backend lives in vmlib/local (Linux, on the host).

vm, _ := l.Create(ctx, vmlib.CreateRequest{ImageID: "ubuntu-base", Profile: "small"})
info, _ := l.SSHInfo(ctx, vm.ID)          // host/port/user + bastion for ssh -J
_, _ = l.Stop(ctx, vm.ID)                  // graceful, idempotent
img, _ := l.Commit(ctx, vm.ID, "golden")  // NEW image; never overwrites
_, _ = l.Delete(ctx, vm.ID)                // frees disk only when exit is proven
```

`Service` and the facade client both implement `vmlib.Lifecycle`, so consumers are
written once for local, remote-backend and remote-facade composition.

## Semantics worth knowing

* A call's `context` bounds that call only. Cancelling a `Create` RPC after the VM
  started does not stop it; use `Stop`/`Delete`. The library installs no signal
  handlers (`vmd` and `vmctl` own them).
* `Stop`/`Delete` are idempotent and serialized per VM. `Commit` requires the
  original VMM's exit receipt, publishes with no-replace semantics and never opens
  the base image writable.
* Anything uncertain (shutdown, identity, configuration, restart) sets
  `retained` on the VM and keeps the lease; `Delete` then returns
  `vmlib.ErrRetained`. Check `VM.Retained` / `RetainedReason`.
* A local backend owns its roots exclusively until `Close()`; `Close` never stops VMs.
  A second backend over the same (or overlapping/symlinked) roots fails at `New`.
* `Create` over the facade takes a client-allocated ID; after a failed or lost
  response, `Get`/`Delete` that ID. `VM.Retained` is a flag over the lifecycle state.
* `WaitSSHReady` uses a prober. The default dials TCP, which only works where the
  host-private guest IP is routable (on the host). A remote client should pass
  `vmlib.WithSSHProber` that goes through its bastion (e.g. `ssh -J ... true`).

## Deployment shapes (`vmd -config FILE`)

* **backend-only**: `backend.kind=local`, `serve_backend` (own CA/cert/key).
* **facade-only**: `backend.kind=local|remote`, `serve_facade`.
* **combined**: local backend with both listeners; the two listeners use separate
  PKI so a facade credential cannot open the more privileged backend.

There is deliberately no config for plaintext, skipping verification, or
exposing the raw Firecracker socket. Raw backend serving is off by default and
only a *local* backend can be served raw.

## Backend contract and trust

See ADR sections 3 and 7. In short: opaque validated IDs, server-owned roots and
profiles, no paths/commands/addresses from callers, mTLS everywhere, trusted-operator
(not multi-tenant) authorization.

## Migration and rollback

Additive. The legacy CLI, `firecracker-builder@main.service`, HTTPS build manager
and `deployment/remote.md` procedures are unchanged and remain the fallback. To
adopt: build `vmd`, review `deployment/multivm`, operator installs. To roll back:
stop `vmd`, remove the new units/helpers (see `deployment/multivm/README.md`).
Existing images in the legacy `images/` tree are plain ext4 files and can be
copied into the new images directory as `<id>.ext4` (read-only).

## Regenerating protobuf code

`make proto` regenerates `vmlib/gen` with generators pinned in `tools/go.mod`
(buf v1.32.2, protoc-gen-go v1.34.2, protoc-gen-go-grpc v1.4.0); `make proto-check`
(part of `make check`) fails if committed code is stale. Network access is only
needed to populate the Go module cache.
