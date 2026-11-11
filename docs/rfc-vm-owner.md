# RFC: A durable workflow owns each VM

Status: Accepted for implementation; deployment requires a separate activation decision.

## Goal

Simplify the active standalone VM control API by making one long-lived DBOS workflow the sole author of each VM's lifecycle decisions. Keep Firecracker, DBOS Go v1.8.0 and existing HTTP clients. Replace fragmented operation orchestration rather than wrapping it in another manager.

## Scope and non-goals

The active application is `cmd/vmapi`; the Telegram bot is an HTTP client. VM operations currently include Create, Stop, repeated cold Commit, Delete, terminal access and SSH metadata. There is no public Start/Restart/Pause operation. Commit creates an independent immutable image and does not consume the stopped VM.

Do not add a generic actor framework, a second scheduler, a continuous self-healing controller, a new hypervisor, an arbitrary state-machine library, per-sample durable monitoring, or an independent domain-database migration. Retain the existing domain store for identity, admission, image references, operations and read projections where needed. SQLite co-location can be considered later; it is not required for this change.

Do not change privileged deployment, runtime resource layout, image bundle format or backend disk/exit guarantees in this refactor. Do not deploy, restart services or access production credentials/state as part of implementation.

## Ownership and execution

- Exactly one stable VM-owner workflow exists per immutable VM identity. It remains pending between operations and finishes after confirmed deletion.
- The owner receives durable typed commands and invokes finite child workflows. It waits for each child before receiving the next command. Two VM owners remain independent.
- Children perform effects and return typed outcomes. They do not independently decide or finalize application VM phases. Existing operation code must be separated accordingly, not reused unchanged beneath a new coordinator.
- Owner and awaited child must not occupy the same concurrency-1 queue slot. Do not rely on an operation queue as a second serializer when the owner already serializes effects.
- Expected operation failures are values: the owner records the result and remains able to receive a later command. Unexpected terminal owner ERROR is visible and retains resources. Automatic replacement owners or poison-history repair are out of scope.
- Stable command/child IDs and matching request identity prevent duplicate effects and conflicting ID reuse. SDK Send success proves durable delivery, not processing or business success.

## Delete and access: deliberately sequential

Delete waits for the current child and any earlier admitted commands. No preemption, separate priority mailbox, external delete-intent fence or immediate access-revocation guarantee is required. Access may remain available until the owner processes the operation. When Stop/Commit/Delete actually executes, normal terminal cleanup still applies.

Admission must not mutate the VM's applied lifecycle phase or revoke access merely because a command was delivered. A Delete operation can be visible as pending without the VM being presented as already deleting. Avoid maintaining a second lifecycle outside DBOS.

A child timeout does not prove the underlying effect has stopped. Never delete/release a workspace while a prior effect could still use it. Keep existing backend serialization, invocation identity, settled-delete barriers and retained/unknown outcomes. Deadline failure may leave a visible retained resource; do not invent a second cancellation/cleanup coordinator.

## Persistence and observations

DBOS owns durable command consumption, child relationships/results and owner replay. Existing domain records remain useful for owner identity, quotas, shared-image reservations, request idempotency, operation polling and list/get projections. They must not independently drive lifecycle transitions.

Publish an owner-controlled VM projection through the existing API/store and/or DBOS events with a clear authoritative writer. Projection writes replay safely. Ordinary reads must not create a second orchestration path.

SSH setup success is distinct from live readiness. Preserve the released bounded host-side TCP readiness contract; it does not prove authenticated SSH. Live terminal processes/connections are rebuildable local projections, not durable checkpoints. Failed terminal attachment must not claim that a known running VM was never created. Preserve authorization and actual cleanup when effects run, without adding immediate revocation at command admission.

## HTTP and shared-resource compatibility

Keep released endpoints, authentication, principal ownership, common image visibility, resource limits, operation polling and Idempotency-Key behavior. Preserve digest mismatch rejection, including after command completion/resource removal. Do not allow DBOS message deduplication to conceal a changed payload.

Short atomic admission still owns cross-VM capacity, names, image publication targets and image-use reservations. Shared-image deletion remains a separate operation coordinated with references, not a message to an arbitrary VM owner.

Retain conflict behavior for incompatible concurrent ordinary commands unless a concrete compatibility adjustment is documented and tested; Delete may be delivered while a child runs. Do not expose a new unbounded user-command backlog just because the SDK supports one.

## Existing state and upgrades

Existing VM identities, owners, guest resources, image references and completed idempotency results must not be replaced or lost. Support bootstrap/adoption of existing settled records without a fresh Create. Do not claim that an existing prepared workspace is a successful running Create.

Define a conservative, explicit handoff for pending legacy operations. Old and new executors must never concurrently own the same VM. If a state cannot be safely handed off, reject startup/migration with a useful diagnostic rather than silently dropping work, clearing DBOS or creating a replacement VM. Avoid preserving the entire old controller as a second active path solely for migration.

Pin workflow names/version policy and document replay-compatible changes. The owner may live across releases; constant ApplicationVersion alone does not make a code change compatible. Use supported DBOS Patch where appropriate and avoid nondeterministic decisions during replay. No automatic history rollover is required for this iteration.

## Code to replace, not supplement

The active VM admission/execution/repair paths in `internal/vmbot/control.go` and DBOS wiring in `internal/vmbot/orch.go` are the primary replacement scope. Remove replaced per-operation lifecycle decisions and startup refresh/resubmit machinery when their responsibility belongs to the owner. Remove dead legacy branches where safe; preserve shared helpers and explicit migration boundaries.

Keep the public vmlib concurrency and safety contract in this iteration. Removing its stateful facade requires a separate compatibility decision. Do not hide a new generic manager behind the owner abstraction.

## Acceptance

1. Real API/controller with real DBOS file SQLite and existing fake lifecycle: Create, Stop, repeated Commit, terminal/SSH and Delete; public client polling remains compatible.
2. Same-VM effects never overlap; two VMs progress independently. Block a child, deliver Delete, verify no admission-time revocation/phase mutation and no disk release before prior work settles.
3. Duplicate request attaches to one semantic operation; changed payload under the same key is rejected before any destructive side effect. Finished/failed owner cannot silently accept a command as completed.
4. Real process SIGKILL/restart on the same database/store: idle owner, child effect before checkpoint and child completion before projection. Stable identities and retained outcomes preserve safety; no claim of exactly-once host effects.
5. Expected child failure leaves owner usable; terminal owner ERROR is visible, does not imply Deleted or free capacity, and has no automatic parallel replacement.
6. Existing settled VM bootstrap preserves identity without Create; pending legacy handoff/rejection is explicitly tested. Replay-compatible upgrade policy is documented and exercised where applicable.
7. Existing authorization/quota/common-image/no-overwrite/clean-exit regression coverage remains. Update tests only for the explicitly changed sequential Delete semantics; do not weaken unrelated invariants.
8. Run repository checks, focused race suites and build binaries. Report exact commands, evidence boundaries and remaining limitations. No live Firecracker/deployment claim based on fake tests.
9. Report the old active orchestration paths actually removed and the remaining authoritative state writers. A new owner wrapped around unchanged phase logic does not satisfy the goal.

## Implementation sequence

1. Establish baseline tests and add failing owner/command/deferred-Delete contract tests.
2. Implement owner and finite effect children, with typed failure results and stable IDs.
3. Wire admission, operations and projections; remove replaced VM lifecycle execution/repair paths.
4. Implement conservative existing-state handoff and terminal projection handling.
5. Exercise process recovery, API compatibility and regression/race suites; update architecture/OpenAPI notes and record limitations.

Deliver in small linear commits, with this RFC preceding implementation. No live activation is implied.
