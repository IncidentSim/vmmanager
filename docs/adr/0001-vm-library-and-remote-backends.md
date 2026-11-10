# ADR 0001: Public VM lifecycle library with local and remote host backends

Status: Accepted
Date: 2026-11-10
Baseline: commit `d352ee4` (single-VM PostgreSQL image builder over one
externally managed, systemd-run, root Firecracker VMM)

## Context

The current program boots one guest on one fixed socket
(`/run/firecracker-builder/main.sock`), fixed `tap-fcpg`, `172.16.0.2`, one
workspace lock, and installs PostgreSQL by SSH. Its safety core is good and must
be kept: externally managed VMM, per-invocation exit receipts, fail-closed
cleanup, disk retention when identity is uncertain, no-replace publication.

The goal is a SadServers-style flow driven by **external automation** (Ansible or
anything else):

1. create a VM from a base image and obtain SSH access;
2. run an external playbook against the guest;
3. gracefully stop the VM and commit its disk as a **new** filesystem image;
4. start several independent VMs from that image and run another external
   (breakage) playbook against each.

Ansible neither creates VMs nor is implemented here. PostgreSQL is not part of the
library core; the PostgreSQL builder becomes an application on top of it.
"Commit" is an **offline disk filesystem image** only: there is no RAM or live
snapshot. Several VMs must run simultaneously.

## Decision

### 1. Layers

```
 external automation / CLI / legacy builder
        |  Go import            |  generated gRPC (portable client, no KVM SDK)
        v                       v
  vmlib.Service  (lifecycle facade; one implementation of orchestration)
        |  vmlib.Backend  (narrow host-operation interface)
        +-----------------------+------------------------------+
        v                                                      v
  local backend  (vmlib/local)                     remote backend client (vmlib/remote)
  real host resources                                  |  gRPC + mandatory verified mTLS
                                                       v
                                         backend gRPC server -> local backend (on the host)
```

* `vmlib` (public, importable by outside modules; **not** under `internal/`):
  domain types, the `Backend` interface, and `Service`, the single facade
  implementation. Contains no PostgreSQL, no Ansible, no gRPC, no Firecracker SDK.
* `vmlib/local`: the only package that touches host resources (Linux, Firecracker
  SDK, TAP/IP leases, workspace disks, receipts). Linux-only via build tags.
* `vmlib/remote`: generated-gRPC transports in both directions plus mTLS helpers:
  a **backend server/client** (client implements `vmlib.Backend`) and a
  **facade server/client** (client implements the facade API).
* `proto/` + `vmlib/gen/`: protobuf definitions and *tool-generated* Go code
  (pinned generators, reproducibility check in `make check`). Transport DTOs are
  separate from domain types; `vmlib/remote` converts and validates at the edge.
* Applications (`cmd/vmd`, existing CLI, legacy HTTPS build API) sit on top.

Because `Service` depends only on `Backend`, the **same facade code** runs
(a) locally over the local backend, (b) on a client over the remote mTLS backend,
and (c) on a server that exposes it as the facade gRPC API. There is no second
orchestration implementation with different safety semantics for remote vs local:
all stop/commit/delete ordering, receipt checks and no-publish rules live in
`Service` plus the backend's own guards (below).

Supported deployment shapes of `vmd`: **backend-only** (host operations over
mTLS), **facade-only** (facade API, using a local *or* remote backend),
**combined** (both listeners in one process, separate TLS configs/ports).

### 2. Why a raw Firecracker socket proxy is insufficient

The Firecracker HTTP API controls exactly one VMM. It cannot copy or publish
disk files, allocate a host TAP device or guest IP, start a VMM process through
the installed launcher, nor read the launcher's root-owned exit receipts. A
proxy would force the client to do those host actions itself (paths, commands),
which is the arbitrary-path/host-command surface we refuse to create. Therefore
the remote **backend contract** is the set of required host operations below, not
a socket forward.

A raw SDK transport/proxy is **not offered by default**. If an operator adds one
later it must be an explicit, trusted-only opt-in (separate listener, off by
default), documented as exposing only the VMM HTTP API, never as an
implementation of `Backend`.

### 3. Backend contract (exact boundary)

All inputs are opaque IDs or validated tokens; no caller-supplied filesystem
path, command, argv, interface name, IP, MAC or socket path ever crosses the
boundary.

| Operation | Meaning |
|---|---|
| `Capabilities` | server policy: max VMs, profiles, limits, bastion metadata |
| `ListImages` / `GetImage` | server-owned image catalog by image ID (`^[a-z0-9][a-z0-9-]{0,62}$`) |
| `Prepare(vm_id, image_id, profile)` | admission check, lease TAP+IP+MAC+socket slot, copy image into a bounded workspace disk (reflink/sparse copy). Idempotent by `vm_id` |
| `Start(vm_id)` | launch an independent VMM through the installed launcher contract, configure it, InstanceStart |
| `Inspect(vm_id)` / `List` | state, guest IP, VMM identity, retained-disk flag |
| `RequestShutdown(vm_id)` | graceful guest shutdown request (SendCtrlAltDel); never a kill |
| `WaitExit(vm_id)` | blocks until the **original** VMM's exit receipt is read and valid |
| `Publish(vm_id, new_image_id)` | requires a confirmed clean receipt; no-replace atomic publication of a **new** image; original base never opened writable |
| `Release(vm_id)` | delete workspace disk and free leases; only with receipt (or VM never started); otherwise refused and disk retained |

Server policy (profiles, caps, roots, image catalog, bastion name) is configured
on the host. Clients choose only a profile name from the advertised list.

### 4. Lifecycle facade (`vmlib.Service`)

`Create(image_id, profile, labels)` -> VM; `Get`; `List`; `SSHInfo`;
`WaitSSHReady`; `Stop` (graceful, idempotent); `Commit(vm_id, new_image_id)`
(stops if needed, waits receipt, publishes, never overwrites); `Delete`
(idempotent; refuses to free a disk when exit is unproven and reports it as
**retained**). Create from image = prepare + start.

**Context semantics.** A context bounds the *call*, never the VM's lifetime.
Cancelling or finishing the Create RPC must not kill a running VM. Cancellation
during Create *before* InstanceStart rolls back safely; after InstanceStart is
possibly issued the VM is left running (state `running`/`uncertain`) and the
caller reconciles with Get/Stop/Delete. The library never installs process-global
signal handlers; the CLI/daemon owns signals. A VM's lifetime ends only through
Stop/Delete/Commit (or guest exit).

**Concurrency.** Per-VM serialization with a state machine
(`preparing -> ready -> starting -> running -> stopping -> stopped ->
committed|deleted`, plus `failed` and `retained`). Concurrent Stop/Stop,
Stop/Commit and Delete/Delete converge on one underlying action and return the
same result. Resource admission is atomic and cap-bounded (a VM slot count and
aggregate memory/vCPU budget), so races cannot over-allocate or reuse a TAP/IP.

**Retained unsafe disks are explicit.** Any path where identity, shutdown or
configuration is uncertain sets `retained=true` with a reason and holds the
lease; Delete returns a typed `ErrRetained`. Nothing frees a disk on uncertain
process state.

### 5. Local backend (host resources)

* One separate Firecracker OS process **per VM**, started by an explicitly,
  externally installed launcher/service contract (new template unit
  `firecracker-vm@.service` driven by a new narrow privileged helper; see below).
  Separate processes are inherent and differ from the legacy per-build self-exec.
* Per-slot unique socket directory, TAP (`tap-vmN`), a /30 inside an
  **operator-chosen** pool (`PREFIX.N.0/30`, guest `.2`, from the root-owned
  `/etc/firecracker-vms/pool.conf`, never Docker's `172.17/16`), MAC
  (`06:00:A:B:N:02`, which the CI Ubuntu rootfs maps to that IP) and jail disk
  path, all derived from a server-allocated slot number, never from user input.
  The network script refuses any existing non-default route or interface address
  overlapping the slot /30, and `vmd` refuses to start if its config disagrees
  with the pool file.
* The lease spans configuration, guest lifetime, exit proof and publication or
  deletion. A responding socket does **not** prove an old VMM exited; only the
  invocation-ID receipt does (same scheme as `cmd/service-launcher`).
* Admission respects the existing bounds: the shared 20 GiB storage filesystem
  and the aggregate 2 GiB / 1 CPU VMM slice. Defaults are conservative
  (e.g. at most 2 guests x 512 MiB, 1 vCPU each inside the 2 GiB slice
  including VMM overhead), configured by the operator, not requested by clients.
  The old 1 GiB per-guest figure is not assumed available to every guest.
* **Restart reconciliation.** State is stored in per-VM metadata files in the
  server-owned root. On start the backend reads them; for each VM it checks the
  receipt: present -> terminal and recoverable; absent while the VMM may still
  run, or metadata unreadable -> VM marked `retained/uncertain`, lease kept,
  nothing freed. Fail closed.
* No SIGKILL fallback. No normal library or CLI path reaches arbitrary host
  services.

### 6. Privileged boundary (proposed, narrow)

The only privileged request is `start-slot N`. It is carried over a
**socket-activated, group-restricted unix socket** (`fc-vm-helper.socket`,
`root:<group> 0660`, `Accept=yes`): the unprivileged `vmd` runs
`vm-helper-client start-slot N`, systemd starts one `cmd/vm-helper` (root) per
connection, which validates a single 63-byte line, requires `N < MAX_SLOTS` from
`pool.conf`, and starts `firecracker-vm@N.service` only if its ActiveState is
`inactive`/`failed`. There is no stop, restart, kill, path, unit-name, interface
or command verb, no sudoers/polkit rule and no blanket systemctl right; a
root-owned script alone cannot elevate, so the socket's owner/group/mode is the
authorization boundary and is delivered as a reviewable unit file. Delivered as
**source templates only**; operator approval is required to install or activate.

`RootDirectory=` applies to every `Exec*` line of the VMM unit, so networking is
**not** run from that unit. TAP/NAT is a separate host-root oneshot,
`firecracker-vm-network@N.service` (`Requires=`/`After=` from the VMM unit, hence
stopped after it and after its exit receipt), and the jail contains no
shell/ip/iptables. Nothing in the library runs sudo or systemctl.

The new multi-VM deployment is **separate** from the active legacy service:
different unit names, runtime directory (`/run/firecracker-vms`), socket
directory, TAP prefix (`tap-vm<N>`), address range, and its own workspace
subtree on the same bounded 20 GiB filesystem so budgets stay shared and are not
silently bypassed. Rollback = disable the new units; legacy service untouched.

### 7. Remote transport and trust

* gRPC over TLS 1.3 with **mandatory mutual authentication**: server requires and
  verifies a client cert against the configured CA; client verifies the server
  chain **and hostname**. No `InsecureSkipVerify`, no plaintext fallback, no
  option to disable either. Misconfiguration is a startup error.
* Trust model: trusted operators, **not** multi-tenant auth. Any client cert
  signed by the CA is fully authorized for that listener.
* **Separate trust domains.** The backend listener (more privileged: it can
  allocate host resources and publish images) and the facade listener each take
  their own CA/cert/key and address. Raw backend exposure is **off by default**:
  `vmd` serves the backend only with `-serve-backend`. Combined mode uses two
  listeners and two TLS configs.
* PKI private keys live outside Git (existing `deployment/generate-mtls.sh`
  conventions); tests use ephemeral in-memory PKI.
* The server never trusts a client to name host paths; it maps IDs to
  server-owned roots after validation, and rejects over-limit or unknown
  profiles by policy.

### 8. SSH contract

`SSHInfo` returns structured data usable by `ssh -J` and an Ansible inventory:

```
host (guest private IP), port, user,
auth: { mode: "client-key-reference" | "provision-public-key" | "image-preinstalled",
        key_reference (a name/label the *client* resolves locally; never a path on the
                       server and never private key bytes),
        public_key_provisioned (bool) },
host_key: { fingerprint (SHA256) | "" , policy: "pinned" | "accept-new-per-vm" },
bastion: { host, port, user }   // from server policy, optional
```

* The guest IP is host-private and unique per VM; reachability is by standard
  `ProxyJump` through the host bastion: `ssh -J bastionuser@host guestuser@172.16.x.y`.
  The service does **not** fabricate remote routing and does **not** create or
  provision the user's bastion SSH account.
* Server provisioning key vs client key reference are distinct fields. The
  server does not pretend client-local paths exist on the server.
* No CA key or server private key is ever in API output; the API never returns
  shared private SSH keys.
* The demo base ships a shared preinstalled SSH key (`image-preinstalled`). That
  is a documented trust limitation: anyone with that key can log into every VM
  derived from that base. Server policy may require public-key provisioning
  instead (injected into a workspace copy only, never the base), but
  provisioning is optional and the base's login compatibility is not broken by
  forcing key regeneration.
* Host-key handling: the guest's host key is unknown before first boot; the API
  reports the policy and, when the server can learn it safely, a fingerprint.
  Default guidance is a per-VM `UserKnownHostsFile` with accept-new, not global
  `StrictHostKeyChecking=no`.
* Examples for external automation are documented but Ansible execution is not
  implemented.

### 9. API shape

Two gRPC services, separate protos and packages:

* `vmlib.backend.v1.HostBackend`: `Capabilities, ListImages, GetImage, Prepare,
  Start, Inspect, List, RequestShutdown, WaitExit, Publish, Release`.
* `vmlib.facade.v1.VMService`: `CreateVM, GetVM, ListVMs, GetSSHInfo, StopVM,
  CommitVM, DeleteVM, ListImages`.

Generated with pinned `protoc-gen-go` and `protoc-gen-go-grpc` driven by a pinned
`buf`; Go code is committed and `make check` verifies regeneration is clean.
Errors map to gRPC codes (`InvalidArgument`, `NotFound`, `AlreadyExists`,
`ResourceExhausted`, `FailedPrecondition` for retained/uncertain) and are mapped
back to `vmlib` sentinel errors on the client so callers use `errors.Is`
identically for local and remote.

### 10. Existing applications

* The PostgreSQL CLI and the legacy HTTPS build API stay working. The CLI keeps
  using the old single-VM externally managed service by default; migration to
  the library is done as an *application* (`cmd/pgbuilder` style example) over
  `vmlib.Service`, with compatibility tests, without removing the old path.
* The legacy HTTPS manager keeps invoking its executable as a child (child
  isolation is retained for that API). The new library does **not** require
  self-exec.

## Consequences

* More code and a generation step; mitigated by pinned tools and a clean-regen
  check.
* A new privileged helper and unit templates must be reviewed by the operator
  before any activation; this ADR authorizes no deployment.
* Concurrency is bounded by the shared 2 GiB / 1 CPU budget, so "multiple VMs"
  means a small, operator-configured number of small guests.
* Fake-backend tests prove orchestration and transport behavior, **not** that a
  real VM boots. Real multi-VM verification is an operator-run script and is
  recorded separately.

## Non-goals

RAM/live snapshots; multi-tenant authentication or quotas; automatic Ansible
execution; automatic user/bastion SSH account provisioning; routing from outside
the host to guest networks; PostgreSQL inside the library core; changing or
restarting the active legacy VMM service.

## Rollback

All new code is additive. Remove the new units/helper (operator action) and stop
`vmd`; the legacy `firecracker-builder@main.service`, CLI and HTTPS build API
are unchanged and remain the fallback.

## Amendments after independent review (2026-11)

* **Ownership.** A local `Backend` takes exclusive non-blocking `flock`s on a lock
  file in each of WorkRoot, RunDir and ImagesDir (canonical paths; overlapping
  roots are rejected) for its whole lifetime. A second owner of the same roots
  fails at `New`, so slots/IPs cannot be double-allocated. Lock files are never
  unlinked. `Close()` is explicit and idempotent, releases the locks only and
  never stops a VM or removes state; operations after `Close` fail.
* **Retention is a flag.** `HostVM.Retained` is orthogonal to the lifecycle state
  (`prepared|running|exiting|exited`). A restarted or uncertain VM with a recorded
  invocation id is asked to shut down (identity-checked, never a replacement);
  with an unknown invocation the backend refuses to signal anything and refuses
  to release.
* **Launch intent is durable.** `Start` persists "launching, retained" before the
  helper runs and then runs to completion regardless of the caller's context (a
  cancelled helper can lose its acknowledgement). After an ambiguous failure the
  lease, slot and disk are kept; the operator procedure is in
  `deployment/multivm/README.md`.
* **No stale deletes.** Workspace entries carry a tombstone checked under the
  entry lock; a delayed `Release` that looked up an old entry reports "already
  released" and never touches a workspace re-created under the same ID.
* **Durability.** Metadata, workspace creation, image publication and the launcher's
  receipt fsync their containing directories. The "committed" classification
  marker is made durable before the image appears and its failure publishes
  nothing.
* **Admission is explicit policy, not the slice.** `max_vms`, `max_total_mem_mib`,
  `max_total_vcpu` and per-profile `mem + vmm_overhead <= unit_mem_limit` are
  enforced under the table lock and counted for retained VMs too. The instances
  share the legacy `system-firecracker\x2dbuilder.slice` (2 GiB / 1 CPU), which is
  a ceiling that kills on overrun, not admission; the policy must be set inside it
  and the legacy VMM's budget share.
* **Storage guard.** `storage_mount` must be a real mount point and the work and
  images roots must be on that device; checked before anything is written. The
  unit repeats the guard with `AssertPathIsMountPoint`/`RequiresMountsFor`.
* **Facade contract.** The client allocates VM IDs so a lost or ambiguous `Create`
  response can always be reconciled; errors carry machine-readable metadata (VM id,
  retained flag, delete result). `ssh_key_reference` is a first-class non-secret
  field. A server-side operation timeout lets a retained result reach a client
  with a longer deadline.
* **Guest network metadata.** `HostVM`/`SSHInfo` carry `GuestNetwork` (address,
  gateway, DNS, and whether the base derives its address from the MAC or is
  configured externally). The supported base contract is documented in
  `docs/ssh-automation.md`; PostgreSQL stays outside the library.
* **Readiness.** `Lifecycle.WaitSSHReady` exists on every implementation. "Ready"
  means what the configured prober proves: the default TCP prober is port-open
  only; `SSHCommandProber` proves an authenticated login through the bastion.
* **Module path.** `github.com/petruekhin/vibe-firecracker`. A real `go get` works
  only after publication; until then `make consumer-smoke` proves importability
  through a local `replace`, and the portable clients cross-build for
  darwin/windows.
