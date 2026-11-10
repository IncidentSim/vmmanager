# ADR 0004: DBOS Transact as the single durable orchestration layer of vmbot

Status: Accepted (repository only; nothing deployed or activated)
Date: 2026-11-10
Builds on: [ADR 0003](0003-vmbot-multi-vm.md); evidence in
[`dbos-evaluation.md`](../dbos-evaluation.md), scope in
[`dbos-orchestration.md`](../dbos-orchestration.md)

## Context

ADR 0003 serializes operations with an in-process per-VM mutex and relies on
"persist intent, then act" plus restart reconciliation. The operator wants a real
durable workflow engine for the lifecycle commands without a second scheduler
next to it. The isolated spike (real `SIGKILL` runs) showed DBOS Transact for Go
v1.8.0 + SQLite gives embedded durability, per-VM partitioned serialization,
idempotent workflow IDs and crash recovery, and also its limits: no exactly-once
external effects, no FIFO guarantee, silent reuse of a finished workflow ID, and
re-execution of a live body when a second instance shares the database.

## Decisions

### 1. Dependency and toolchain

`github.com/dbos-inc/dbos-transact-golang v1.8.0` with its pure-Go SQLite driver
(`dbos/driver/sqlite`, `modernc.org/sqlite`, no cgo). The main module requires
`go 1.25.0` (DBOS's own floor). The OS toolchain is not touched: `GOTOOLCHAIN=auto`
fetches the toolchain on demand. `vmlib` imports nothing from DBOS, so the library
API and behaviour are unchanged; only its module's language version moves.

### 2. One scheduler: the DBOS queue replaces the per-VM mutex

`Controller` keeps its public API (`Create`, `Delete`, `Terminal`, `Status`,
`List`, `Recover`) and the bot is unchanged. Underneath, **every** lifecycle
command and every recovery action is a DBOS workflow on queue `vmops` with
partition key = VM ID and partition concurrency 1; unrelated VMs run in
parallel. The in-process per-VM lock is deleted. A command handler does only:
validate/resolve, persist the durable intent in the domain state, perform the
local security revocation where applicable, submit with a deterministic workflow
ID, and wait (bounded) for the result. There is no code path that performs a
remote operation outside a workflow, and no second serializer.

Queue concurrency is mutual exclusion, **not** command order. Causality
(create before delete, delete before terminal) comes from durable VM phase
checks inside every workflow body, never from queue order.

### 3. Domain state stays JSON; DBOS SQLite is an execution log only

`state.json` (schema v2, the ADR 0003 file) remains the **only** authority for
domain data: VM records, tokens, generations, phases, labels, the Telegram
offset, and the per-VM operation sequence. DBOS's `dbos.db` (SQLite, real file
`<state_dir>/orchestration/dbos.db`, 0700 directory, files 0600; `:memory:`
and relative paths are refused, there is no in-memory fallback) records
workflow progress only. Workflow inputs and outputs contain IDs, kinds and
sequence numbers and a small result code/status; never tokens or links. Because
the durable intent (record phase, sequence) lives in the JSON file, a lost or
refused `dbos.db` loses no domain truth: startup recovery re-submits the intent
of every record. A corrupt `dbos.db` makes startup fail closed; it is never
deleted automatically. Two authorities for the same fact are avoided by never
copying domain fields into DBOS.

The v1 -> v2 migration (ADR 0003) is unchanged: deployed VM ID, token,
generation, phase and offset are preserved and `state.json.v1.bak` (0600, never
overwritten) is kept. v2 gains the optional `seq` field per record
(`omitempty`, so existing v2 files and migrated records stay valid).

### 4. Intent and workflow IDs

| Command | Durable intent written first | Workflow ID |
|---|---|---|
| create | the `creating` record (admission: capacity, unique name, generation) | `create-<vmID>` |
| delete | phase `deleting` + `seq` | `delete-<vmID>-<seq>` |
| terminal, status/reconcile | none (projection) | `terminal-<vmID>-<random>`, `reconcile-<vmID>-<random>` |

* A crash after the intent but before enqueue is repaired at startup: `Recover`
  re-submits every `creating` and `deleting` record with its deterministic ID
  (DBOS returns the existing execution if there is one), and reconciles every
  `active` record, which re-attaches its terminal.
* DBOS silently returns the old execution when an ID is reused, even for other
  input. Submission therefore compares the stored workflow input with the
  intended one and refuses a mismatch (`ErrFailed`, never retargeted).
* A failed or retained delete finishes its workflow with a *result*; retrying
  `/delete` finds the finished workflow for the current `seq` and persists
  `seq+1` before submitting `delete-<vmID>-<seq+1>`. A cached failure is never
  replayed forever. A duplicate `/delete` while one is queued or running attaches
  to the same workflow (one body execution).
* Step retries inside DBOS are disabled: outcomes (failed, retained, uncertain)
  are values reconciled by the existing ADR 0002 rules (`Get`, delete barrier,
  `NotFound` never proves absence, `ErrRetained` keeps the record).
* A terminal SDK outcome (`ERROR`, `CANCELLED`, exhausted recovery budget) is
  not `Busy`. When its status is positively final, a fresh reconcile workflow
  adopts an existing remote VM or applies the successful Delete absence barrier.
  It never retries Create under a new ID. Unknown/unreadable workflow state stays
  occupied; startup reports unfinished recovery rather than silently succeeding.

### 5. Effects may repeat; identity is stable

DBOS replays a step if the process dies between the effect and its checkpoint.
Every effect is safe to repeat: the VM ID is the stable external identity;
create does `Get` first and adopts an existing VM; delete is vmlib's idempotent
barrier. The create body re-reads the durable phase first, so a recovered
create never resurrects a VM whose delete intent was persisted first.

### 6. Terminals are runtime projections

Open ttyd/ssh/WebSocket sessions are process state, not checkpointed resources.
The local terminal manager (slots, cleanup barriers, token reservation; ADR
0003) is unchanged and DBOS does not replace it. Rules:

* The delete command persists `deleting` **before** anything is enqueued and
  revokes the local terminal immediately (outside the queue, because queue
  position may be behind a long create); the delete workflow revokes again.
* Every terminal-granting step re-reads the durable phase, grants only for
  `active`, and re-checks after starting the backend, revoking at once if a
  delete intent appeared in between.
* A post-start check alone is insufficient: the backend could be reachable in
  the interval before that check. Each controller-created terminal target also
  carries a live authorization fence bound to VM ID, generation and token. The
  manager checks this fence before startup, at publication, on HTTP lookup and
  at WebSocket admission/hijack. A durable `deleting` intent therefore refuses
  new access even while local process cleanup is still pending.
* Slow local revocation runs outside the shared delete-intent gate. Both the
  command and the workflow drain the same manager slot idempotently, but
  teardown of one VM cannot block another VM's delete admission.
* After a crash, recovery rebuilds projections from durable phase: `active` and
  running -> reattach; `deleting` -> revoke, never restart.

### 7. Single instance, fixed executor, application version

The state-directory lifetime `flock` (taken in `OpenStore`) is acquired before
DBOS `Launch`; the executor ID is the constant `vmbot` and the application
version a constant (not the binary hash) so recovery keeps working across
rebuilds. A second instance on the same database re-runs live bodies (spike), so
it must never start: `OpenStore` fails with `ErrLocked` first.
The Store also leases its lock to one controller engine, preventing two engines
from sharing one already-open Store. `Store.Close` refuses to release the flock
until DBOS shutdown succeeds. If shutdown times out, retain the lock until process
exit rather than allowing a successor to recover work still executing locally.
The engine reference is published before `Launch` can recover workflow bodies
and is not cleared underneath draining work.

### 8. Testing

Normal `make check` (no root, KVM, network, token) runs: real SQLite files; fake
`vmlib` (in-process `vmlibtest` for logic; a file-backed fake for cross-process
tests); real `SIGKILL` of a worker process after the external effect, after the
delete intent and before enqueue, then restart on the same directories; plus
`ErrRetained`, early `NotFound`, delete vs recovered create, duplicate commands,
ID collision, cross-VM progress, terminal revocation and the v1 migration through
the orchestrated path. The terminal manager regression tests remain.

## Consequences and residual gaps

* A dependency of ~55 modules (spike: pgx etc. are pulled in transitively) and
  Go >= 1.25 for every build of this repository.
* `dbos.db` grows (finished workflows are not pruned); retention is not done.
* Workflow code changes need a new application version/migration plan: pending
  workflows of an incompatible version must be drained first.
* Rollback to the single-VM binary is impossible once v2 state exists (ADR 0003);
  `dbos.db` is simply ignored by older code.
* Not proven offline: behaviour with the real daemon, long-run SQLite behaviour,
  `MaxRecoveryAttempts` poison loops, disk-full/corruption beyond fail-closed
  open.
