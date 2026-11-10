# ADR 0005: Standalone authenticated control API

Status: proposed (source only; not deployed or activated).

## Context

ADR 0003/0004 made `vmbot` a multi-VM Telegram controller that owned the DBOS
state, VM ownership and the terminal manager inside one process serving a single
allowed user. Multi-user operation, non-Telegram clients and per-VM CPU/RAM
need an authorization/quota model that must not live in a chat bot, and two
controllers must never contend for one state directory.

## Decision

1. `cmd/vmapi` (new) is the single owner of durable application state, DBOS
   workflows, per-user ownership/quota, image ownership and terminal
   projections. It talks to the existing `vmlib` facade over mTLS gRPC.
2. `cmd/vmbot` becomes an HTTP-only frontend using `controlclient`. It has no
   orchestration, store or facade access and no embedded-controller fallback.
   It keeps its own private delivery offset/lock (at-most-once update policy).
3. HTTP v1 with bearer auth: user credentials bound to one numeric user ID, and
   a delegated service credential that must name an allowlisted acting user.
   Foreign and unknown IDs are indistinguishable (404).
4. Every mutation needs an `Idempotency-Key`. Identities (VM, image, operation)
   and quota are reserved atomically in `state.json` before DBOS enqueue and
   before 202. No random-ID creation inside a retried workflow step. A startup
   repair pass bridges the JSON/SQLite enqueue gap with stable workflow IDs.
5. Resources are exact CPU/RAM (`vmlib.VMResources`), mutually exclusive with a
   named profile; backend policy is authoritative. Commit is a **cold** disk
   commit into an owner-private immutable image; the source stays stopped and
   occupies capacity until Delete. Images referenced by any VM, and shared
   bases, cannot be deleted; no force removal.
6. Legacy v1/v2 state is adopted only with an explicit `legacy_owner_user_id`
   (exact backup, identity/offset preserved); no guessing.
7. Startup recovery failure does not exit the process: the API serves a safe
   `degraded` health state so retained/unknown operations stay observable.

## Alternatives rejected

* Keep a second controller inside the bot: two writers of ownership/quota and
  two DBOS engines; no safe fallback exists, so none is offered.
* Retry `Create` with fresh random IDs: can duplicate VMs on lost responses.
* Adopt an existing image after `AlreadyExists` on commit: any colliding image
  would become owned.
* A scheduler/task map in the API: duplicates DBOS and loses work on restart.

## Consequences and residual risk

* `Lifecycle.Commit` has no durable provenance across backend restarts, so a
  crash mid-publication ends as a fail-closed `failed` operation with a private
  `publishing` image and manual operator recovery (documented in
  `control-api.md`), not transparent exactly-once recovery. Resolving this needs
  backend-recorded source/publisher provenance (a `vmlib` change).
* Backend profiles and aggregate caps reach `/v1/capabilities` through the
  additive facade `GetCapabilities` RPC (resource policy only, no SSH/bastion
  data). An older vmd without this RPC makes the endpoint return 503.
* Caller SSH public keys are rejected until the backend supports them.
* Bearer terminal links can be forwarded; allowlisting is not guest isolation.
* Recovery is one-shot per process; failed operations keep their slot until an
  explicit Delete; no pruning of finished DBOS/operation records.
* Rollback to the legacy bot after migration is not supported.
* Proven only with fake backends over real loopback mTLS/HTTP and SQLite; no
  Firecracker/KVM, live Telegram, or production run has been performed.
