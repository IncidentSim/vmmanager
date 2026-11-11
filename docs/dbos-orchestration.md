# DBOS orchestration direction

> **Superseded in part.** The active control API now uses one sequential,
> long-lived DBOS owner workflow per VM instead of a partitioned queue of
> per-operation workflows; see the [RFC](rfc-vm-owner.md), the
> [architecture notes](architecture.md#vm-owner-workflow) and the
> [verification report](vm-owner-verification.md). The queue partitioning below
> remains the evaluation record only: the single-tenant (non-control) controller
> path it served has been removed (a retirement stub for old executions remains,
> see [cleanup](vm-owner-cleanup-verification.md)). The evaluation criteria and caveats (no exactly-once host
> effects, stable identities, live terminals are local) still apply.

## Decision and scope

Use DBOS Transact for Go with a local SQLite system database as the proposed
durable orchestration layer for multi-VM application operations. Validate the
runtime contract with an isolated executable spike before integrating it. The
currently deployed single-VM bot must remain untouched during this evaluation;
the repository's multi-VM implementation is not yet deployed.

The existing `vmlib` and Firecracker host backend remain responsible for safe VM
and disk operations. DBOS is not a replacement for systemd, host admission policy,
exit receipts or SSH/WebSocket process ownership. No TTL, summaries or new web
management interface are in scope.

## Relevant documented mechanisms

- Go supports a SQLite system database. Current documentation describes the
  optional pure-Go driver import `dbos/driver/sqlite`.
- Partitioned queues support per-partition concurrency. A candidate design uses
  `vm_id` as partition key and concurrency1 per partition, while unrelated VMs
  can progress concurrently.
- Workflow IDs support idempotent submission. Queue partition keys and queue
  deduplication IDs cannot be combined according to current documentation;
  do not assume both can be enabled on the same enqueue.
- Workflow recovery/checkpointing does not provide exactly-once execution of
  external Firecracker, SSH or filesystem actions. Activities must use stable
  operation/VM identities and reconcile uncertain outcomes.

Check the actual pinned release: current website documentation may describe
APIs newer than a released module. Verify Go toolchain requirements, SQLite
support, queue APIs and license before choosing an integration version.

## Evaluation acceptance criteria

1. Embedded local SQLite execution without a broker or workflow server.
2. No overlapping operation bodies for the same VM across submissions and
   recovery; unrelated VM operations may run concurrently.
3. Forced process death between external effect and checkpoint, then recovery
   against the same database. Stable identity/reconciliation must prevent a
   duplicate external resource. Persist a mock external resource separately
   from the workflow database so replay is genuinely exercised.
4. Repeated submission with one workflow ID must not create duplicate work.
5. Create/delete ordering, retry and failure handling must be explicit. Do not
   assume a concurrency limit establishes command FIFO order or causal ordering.
6. Shutdown/recovery must not reauthorize a terminal revoked by durable delete
   intent. Live terminal cleanup remains a local responsibility; no durable
   engine should claim to persist an open socket or process.
7. Report executed results, dependency/toolchain cost and gaps. Do not replace
   missing SQLite or partition support with an undisclosed in-memory fallback.

## Source and runtime context

- Historical design: ADR0001, ADR0002, ADR0003 and `docs/vmbot.md` (historical pointer).
- Current code: owner runtime in `internal/vmbot`, API in `internal/controlapi`, HTTP-only bot in `cmd/vmbot`.
- Deployment state lives outside the repository under
  `/home/amnezia/firecracker-vms`; never include filled state, tokens or private
  keys in source or evaluation artifacts. Do not access the runtime during the
  isolated evaluation.
- Preserve the existing terminal cleanup/reservation regression tests when
  integrating durable orchestration. They cover real local lifecycle boundaries
  that DBOS alone cannot remove.

## Sources

- https://docs.dbos.dev/golang/reference/configuration#using-sqlite
- https://docs.dbos.dev/golang/tutorials/queue-tutorial
- https://docs.dbos.dev/golang/reference/workflows-steps
- https://github.com/dbos-inc/dbos-transact-golang
