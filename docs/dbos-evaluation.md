# DBOS Transact (Go) + SQLite evaluation

Date: 2026-11-10. Scope: isolated spike only. No application code, runtime,
credentials, VM, Telegram or commit was touched. Mock external resources only.

Spike (git-ignored, own Go module): `.tools/dbos-spike/`
Verbatim results: `.tools/dbos-spike/results/run-final.txt`

## Verdict

**Feasible, with conditions.** On the pinned release DBOS gives embedded SQLite
durability, per-VM serialization with cross-VM parallelism, idempotent workflow
IDs and crash recovery, all verified by real `SIGKILL` runs. It does **not**
give exactly-once external effects, command FIFO/causal ordering, or protection
against two live instances. Those remain the application's job (stable identity +
reconcile, durable phase checks, single-instance lock). The main integration cost
is the Go toolchain jump (1.22 -> 1.25+), not the runtime.

## Pinned release, toolchain, license, cost

| Item | Result |
|---|---|
| Module | `github.com/dbos-inc/dbos-transact-golang v1.8.0` (latest release listed 2026-11-10; v1.0.0 ... v1.8.0 exist) |
| Its `go.mod` | `go 1.25.0`, `toolchain go1.26.10` |
| Repo toolchain | go1.22.2; repo `go.mod` is `go 1.22` |
| Spike toolchain | `go version` inside the spike reports go1.25.0 (fetched by `GOTOOLCHAIN=auto`; the lookup also pulled go1.26.10). Build succeeded with 1.25.0 |
| SQLite | `import _ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite"`, pure Go `modernc.org/sqlite v1.54.0`, no cgo |
| License | DBOS: MIT. modernc sqlite/libc: BSD-3-Clause style. pgx: MIT (pulled in even for SQLite use) |
| Cost | ~55 modules in `go list -m all` (cobra/viper/pgx/modernc included); spike binary 23 MB |
| Enterprise | conductor / auto-upgrade are optional and were not used |

Blocker for direct integration: the main module would have to move from Go 1.22
to >= 1.25 (and the host toolchain upgraded), or DBOS must live in a separate
module/binary. I did not change the main `go.mod`. Older releases were not
checked for lower Go requirements; per-partition limits
(`WithPartitionConcurrency`) are documented in v1.8.0, and the older
`WithPartitionQueue` is deprecated there.

## API actually used (v1.8.0)

- `dbos.NewContext(ctx, dbos.Config{AppName, DatabaseURL: "sqlite:/abs/dbos.db", ExecutorID})`.
  `AppName` must match `^[a-z0-9-_]{3,256}$`.
- `dbos.RegisterWorkflow`, `dbos.RegisterQueue(ctx, "vmops", dbos.WithPartitionConcurrency(1), ...)`,
  `dbos.Launch(ctx)` (recovers this executor's PENDING workflows).
- `dbos.RunWorkflow(ctx, fn, input, dbos.WithQueue(q), dbos.WithWorkflowID(id), dbos.WithQueuePartitionKey(vmID))`.
  A partitioned-limit queue rejects an enqueue without a partition key.
- `dbos.RunAsStep(ctx, fn, dbos.WithStepName, dbos.WithStepMaxRetries, ...)`,
  `dbos.ListWorkflows`, `dbos.Shutdown(ctx, timeout)`.
- Dequeue order in source: `ORDER BY priority ASC, created_at ASC` (created_at in ms).

## Commands

```
cd .tools/dbos-spike
go build -o bin/spike .
go run . drive ./bin/spike ./run          # all scenarios, real processes, exit 0 = all checks pass
go run . drive ./bin/spike ./run s5       # one scenario
```
The driver spawns `bin/spike worker` processes, sends real `SIGKILL`, restarts
them on the same `dbos.db` and the same mock directory (`run/<scenario>/ext`:
`vms/` = mock backend, `state/` = app durable phase, `ops.log` = evidence). The
mock backend is persisted separately from SQLite.

## Results by acceptance criterion (final run: 0 failed checks)

1. **Embedded SQLite, no broker/server.** Worker ran from a single `dbos.db`;
   the process owned 0 listening TCP sockets (counted via own socket inodes).
2. **Serialization / parallelism.** Partition key = VM, concurrency 1: A1..A3,
   B1..B2, C1 (300 ms each) took 1.09 s (serial would be 1.8 s), peak 3 distinct
   VMs in parallel, zero same-VM overlap. Negative control with partition
   concurrency 2 did overlap, so the detector works. Across SIGKILL of a
   worker with A1 and B1 in flight, then restart: zero overlap, A1 body ran
   twice (killed + recovered), order `[a1 a1 a2 a3]`, all SUCCESS.
   **Hazard found:** a second process started against the same DB with the same
   executor ID while the first was alive re-ran the live A1 body (same-VM
   overlap observed). A different executor ID did not re-run it, but then a dead
   executor's PENDING work is stranded. An `flock` lifetime lock refuses a second
   instance (exit 3). A single-instance lock is therefore mandatory.
3. **Checkpoint crash window.** Kill after the external effect and before the
   checkpoint: with stable identity (VM ID, get-or-create) the replayed step
   adopted the resource (new=1, adopted=1, one resource). Negative control
   without stable identity produced two resources. Kill before effect: one
   resource. Kill after checkpoint: the completed step was not re-run (1
   execution). Idempotent delete replay converged. In-process step retries after
   an effect behave the same (stable: 1 resource; naive: 3).
4. **Duplicate workflow IDs.** Six concurrent submits of one ID: one body, one
   resource, one row. Resubmission after completion, even with different input
   (other VM), re-executed nothing and silently returned the old execution: the
   ID must encode the intent (e.g. include VM ID and operation sequence).
   `WorkflowIDReusePolicyReject` yields `ErrWorkflowIDInUse`. Partition key +
   deduplication ID is rejected (`InvalidOption`), so workflow ID is the only
   idempotency mechanism available on a partitioned queue.
5. **Ordering, retry, failure.** Concurrency 1 is mutual exclusion, not a
   command order. Observed: a sequential single-submitter burst of 40 ran in
   submission order, but 15 groups shared the same millisecond `created_at`, and
   with 8 concurrent submitters order differed from file order (4 inversions).
   Source sorts only by priority then ms timestamp, with no documented tie-break
   guarantee, so FIFO is an observation, not a guarantee. A failed create
   (`ERROR`, `MaxStepRetriesExceeded`) did not stop later terminal/delete for the
   same VM; they ran and the terminal was refused only by the app's durable phase.
   Causal create->delete correctness must come from durable VM phase checks
   inside each operation (and, if strict command order matters, from an
   application sequence number or a single per-VM workflow), not from the queue.
   Recovered work was re-run at `Launch` before the application could enqueue
   anything, so a newly submitted higher-priority delete did not overtake it.
6. **Revocation vs recovery.** After a kill following the durable `deleting`
   intent, recovery and a fresh terminal request granted nothing; the request was
   refused from durable phase, and delete completed. When a terminal grant
   step was killed before checkpoint and the app then persisted `deleting`, the
   replayed step re-read the phase and refused. Live terminal tables are process
   memory by design; nothing about sockets or processes is persisted by DBOS, and
   cleanup stays local.
7. **Shutdown.** `dbos.Shutdown(300ms)` with a 2 s in-flight body returned
   `shutdown timed out ... waiting for: workflows`; after process exit the work
   was replayed on restart (attempts=2) and completed. Shutdown does not make a
   long non-idempotent body safe, so bodies must tolerate replay.

## Not proven / gaps

- Not exercised: `MaxRecoveryAttempts` poison-loop behavior, long-term SQLite
  growth/retention, multi-executor on SQLite (SQLite is single-host), database
  corruption/disk-full, upgrade across DBOS versions, application-version
  mismatch on recovery (same binary used throughout), crash during DBOS's own
  SQLite writes beyond SIGKILL.
- Mock resources are files; real Firecracker/vmlib semantics (`ErrRetained`,
  `NotFound` not proving absence) were not simulated.
- FIFO observations come from one machine and small bursts.
- The Go 1.22 -> 1.25 impact on the existing codebase (vet/tests/CI) was not run.

## Integration recommendation

1. Adopt only if the Go >= 1.25 toolchain move is acceptable; otherwise isolate
   DBOS in a separate module/process boundary. Pin v1.8.0 and its SQLite driver.
2. Queue `vmops` with `WithPartitionConcurrency(1)`, partition key = VM ID;
   take the existing lifetime lock before `Launch` and use one fixed executor ID.
3. Workflow ID = deterministic intent key (VM ID + operation + generation), never
   reused for different input; use the Reject policy where a collision is a bug.
4. Each step: persist intent in the app state first, use the VM ID as the stable
   external identity, reconcile via `Get`/`Delete` barrier, treat `NotFound` per
   ADR 0002, and re-read durable phase before authorizing terminals.
5. Keep terminal cleanup/reservation and exit-receipt logic local; keep the
   existing regression tests. Do not rely on queue order for create/delete
   causality.
