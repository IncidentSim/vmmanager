# Owner-era simplification candidates

Read-only audit target: 665cae2. This is a proposed cleanup, not a record of deletions.

## First cut: retire unused single-tenant application

The current production chain is cmd/vmbot -> internal/tgbot -> controlclient ->
HTTP, and cmd/vmapi -> internal/controlapi -> internal/vmbot control/owner.
The former internal/vmbot Bot and its own Telegram/config implementation are not
used by these entrypoints.

1. Remove internal/vmbot/{bot,telegram,config}.go: 812 physical production lines.
   Remove their obsolete frontend/config/deploy tests and legacy config fixtures.
2. Remove legacy Controller Resolve/Recover/List/Status/Create/Terminal/Delete,
   selector/label helpers, intentMu and legacy-only errors/config fields: roughly
   374 lines. Preserve active Controller constructor/engine locking, terminal
   interfaces, ensureTerminalLocked, identity helpers and active errors.
3. Remove legacy createFlow/deleteFlow/terminalFlow/reconcileFlow, queue and
   submission/wait chain from orch.go: roughly 527 lines plus wiring. Preserve
   Start/Close, open-store/exclusive engine ownership, DB location/config,
   owner/effect/image registration. Keep a small retirement registration stub
   until recovery without the old workflow registration has been tested.
4. Remove state ByGeneration/BySelector after legacy callers are gone: 22 lines.

Measured gross production candidates: approximately 1,735 lines before retaining
small migration stubs. Physical counts include comments and blank lines. This
exceeds the owner implementation's +626 non-test source-line increase, but net
cleanup has not yet been implemented or verified.

## Keep actual compatibility and safety tests

Extract shared fakeTerms/hookLC fixtures from controller_test.go before deleting
legacy tests. Retain engine exclusivity, lifetime flock, Start-on-closed-store
checks, terminal applied-phase authorization checks, owner/control/SSH/image
checks, migration and previous-application-version isolation. Roughly 2.7-2.9k
obsolete test lines could disappear after keeping these shared fixtures/checks;
this is an estimate, not an acceptance target.

JSON fields Attempt/Repair/Seq/Offset/Exec/Control/generation/token/owner are not
mechanically dead: strict decoding and handoff/migration still depend on them.
Keep OpenControlStore migrations/backups, checkHandoff, legacy delete synthesis
and abandonment/adoption. Do not relax unknown-field decoding to hide migration.

## Follow-up local simplifications, not prerequisites

* Owner Stop before Commit may duplicate Service.Commit's stop/clean-exit work.
  Confirm/document Lifecycle.Commit semantics and test alternate implementations
  before removing the redundant outer call; retain Service's safety checks.
* Remove only redundant tombstone rewrites after the first Delete reservation,
  never the early reservation or settle barrier.
* Reuse operation lookup inside applyOutcome's same atomic store transaction.
* Reuse one snapshot for ordinary list projections, not SSH readiness's required
  post-probe phase/state rechecks.
* Remove legacy non-control branch from terminal authorization only after deleting
  that runtime; preserve live ownership/phase authorization and cleanup barriers.

## Non-goals

Do not delete public vmlib concurrency/monotonic state guards, backend allocation
locks, publication exclusion, invocation/receipt verification, uncertain Prepare
retention, Delete settle barrier or Close drain. Reads/public Go callers do not
become serialized merely because API mutations use an owner. Do not change owner
durable-call history or persisted input/output shape as incidental dead-code work.

## Documentation

Correct architecture's old cmd/vmbot -> internal/vmbot description. Replace old
vmbot/dbos runtime guides with explicit historical pointers or archive them;
retain ADRs and verification evidence. Unrelated image-builder/remote-builder and
privileged helper executables are outside this cleanup's scope.

## Acceptance for a future cleanup

Use a separate worktree from the live-verified owner revision; no service changes.
Delete coherent unused chunks, retain active API/recovery/migration tests, run full
uncached make check and independent diff review. Measure actual net production
lines removed, compare public API/schema and preserve loaded old JSON. Rebuild
binaries and exercise the cleaned owner with real guests separately before any
activation. This audit itself neither authorizes nor performs those deletions.
