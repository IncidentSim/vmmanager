# Legacy single-tenant vmbot removal: verification

Scope: remove the unused single-tenant frontend/controller/workflow code
identified in [vm-owner-cleanup-candidates.md](vm-owner-cleanup-candidates.md).
Baseline: `6ddb2bd`. Source-only cleanup; no deployment, service, root or
live state/config changes. Public API, auth, quotas, images, terminal, SSH and
runtime safety behavior are unchanged.

## Removed mechanisms

Production (`internal/vmbot`, plus one argument in `cmd/vmapi/main.go`):

* `bot.go`, `telegram.go`, `config.go`: the old standalone Telegram frontend,
  Bot API client and JSON config loader. `cmd/vmbot` never used them; it depends
  only on `controlclient`, `internal/tgbot`, `internal/identity`, `internal/jwtauth`.
* `controller.go`: `Resolve`, `Recover`/`recoverOne`, `List`, `Status`,
  `Create`, `Terminal`, `Delete`, `reconcileLocked`, `confirmGoneLocked`,
  `pickLabel`, `statusOf`, `query`, `setPhase`, `drop`, `Ref`, `MaxVMs()`,
  `opWait`, `intentMu`, `DefaultMaxVMs`, the legacy-only errors (`ErrBackendFull`,
  `ErrNameTaken`, `ErrBadName`, `ErrNeedSelector`, `ErrBusy`, `ErrStale`) and the
  unused `ControllerConfig.ImageID`/`MaxVMs` fields (`vmapi` no longer passes
  `MaxVMs`; the control policy carries the limits).
* `orch.go`: the create/delete/terminal/reconcile flows, `classifyCreateFailure`,
  the submit/wait/lookup/sameInput chain, delete-attempt sequencing,
  `toResult`/`OpResult.err`, result codes (except `failed`), operation kinds and
  ID helpers, `OrchestrationDBPath`.
* `state.go`: `State.ByGeneration`, `State.BySelector`.
* `testdata/legacy-vmbot.*` (example config, unit file) used only by the removed
  config/deploy tests.

## Deliberately kept

* `Controller` constructor, `Start`/`Close`, engine exclusivity
  (`claimEngine`/`releaseEngine`), the Store lifetime flock, closed-store check,
  `checkHandoff`, owner/effect/image workflow registration and the terminal
  rebuild arming.
* `ensureTerminalLocked`, including its live authorization closure and its
  non-control branch. That branch is only removable once non-control stores stop
  existing; this pass does not change terminal authorization.
* Strict JSON decoding and the old fields (`Attempt/Repair/Seq/Offset/Exec/
  Control/generation/token/owner`), `OpenStore`/`OpenControlStore` migrations,
  backups, legacy delete synthesis and abandonment/adoption.
* **Retirement stub.** The `vmbot-op` workflow name, its `OpInput`/`OpResult`
  shapes and the `vmops` queue stay registered; `vmbotOp` now only returns the
  failed result "legacy workflow retired by control migration" (the text control
  stores already returned). It touches no state or VM.
* Pinned names/version (`orchAppVersion = vmbot-owner-1`, workflow names).
* No change to Stop-before-Commit, tombstones, owner durable-call history or
  persisted input/output shape.

## Why the stub stays (tested, not assumed)

`TestOrchestrationRecoversInterruptedLegacyWorkflowAsRetired` simulates a
previous build of the current application version that left a `vmbot-op`
execution PENDING, then starts this build on the same database. The stub
completes it with the retired result, does not re-run the old body and does not
affect the backend VM. With the registration commented out, this test and
`TestControlMigrationQueuedLegacyDeleteWorkflowHandoff` fail (the execution is
never completed; DBOS logs "Workflow not found in registry"), which was observed
before restoring it. Whether the stub can later be dropped depends on proving no
database anywhere holds such executions; that is not shown here.

## Tests

Removed with the unused code: the frontend/config/deploy tests (`bot_*`,
`telegram`, `config`, `deploy`) and the legacy-controller/orchestration tests
(`controller_test`, `controller_multi_test`, `controller_adversarial_test`,
`orch_concurrency`, `orch_process`, `orch_recovery`, the legacy status test in
`orch_test`, and the `BySelector` assertions in `state_migration_test`). 72 test
functions were removed; 1 added.

Extracted to `fixtures_test.go`: `fakeTerms`, `hookLC`, `env`/`newEnv`,
`diskLC`/`diskTerms`/`waitMarker` (still used by `control_process_test.go`) and a
new `seedActive` helper (writes an active record plus backend VM directly instead
of going through the removed `Create`).

Retained with unchanged assertions: engine exclusivity, lifetime flock, Start on
a closed store (`orch_test.go`); terminal applied-phase authorization
(`terminal_authorization_test.go`, only the setup call changed); owner, control,
image, SSH, migration (`state_migration_test`, `control_legacy_delete_test`) and
previous-version isolation (`owner_version_test`) tests. The queued-legacy-delete
handoff test keeps its assertions but submits through a small test helper
(`runLegacyOp`) that enqueues on the retained queue.

Added: `orch_retirement_test.go` (see above; it now also asserts the store is
unchanged, and no terminal start/revoke occurred) and
`owner_retained_quota_test.go` (coverage map below). `control_process_test.go`
`TestControlRealKillRecovery` regained, for the `delete-intent`/`delete-effect`
modes, the old assertions that the remote fake resource directory is empty after
recovery and that no terminal is ever granted.

### Coverage map for removed legacy assertions

`TestOwnerAmbiguousCreateRetainedQuotaSurvivesFailedDeletesAndRestart` (real
`vmlib.Service`, real controller, owner workflows, DBOS SQLite; two subtests):
ambiguous Create (backend creates the VM, caller gets a wrapped `ErrRetained`)
-> failed operation, record kept with token/generation/owner -> quota (MaxVMs 3)
full, further create `ErrCapacity` -> three unsuccessful Deletes (`ErrRetained`,
`DeadlineExceeded`, `Deleted:false` with nil error), each leaving the record,
identity, backend VM and quota intact and never active -> full close/reopen/
Start/RecoverControl: same record, no repeated Create (exactly 1 per VM ID), no
repeated Delete -> explicit successful Delete frees record, backend VM and quota
(a new create succeeds). No terminal is ever granted to the retained VM/token.
Subtests: Get reports `running` (record is promoted to active: current
behavior) and `preparing` (stays `creating`, never promoted).

| removed legacy assertion | current coverage |
|---|---|
| retained/ambiguous Create keeps the slot | new test; `TestOwnerUnexpectedErrorIsVisibleAndRetained`, `TestControlChildFailureIsOutcomeNotRepair` |
| unknown/retained Delete keeps occupancy, retry frees | new test (three failure kinds) |
| restart never repeats Create / never re-authorizes terminal | new test; `TestControlRealKillRecovery` (creates file, `granted` marker) |
| delete recovery converges to absent remote resource | `TestControlRealKillRecovery` delete modes (restored) |
| generation-bound Delete (`ErrStale`) | not re-added: API removed; control operations are VM-ID/owner bound, see `owner_contract_test` |
| persist failure of the deleting phase changes nothing | not re-covered here |
| ambiguous Prepare keeps the slot, NotFound during pending Create, delete barrier | still only in `vmlib` tests; the new test uses `Get` state `preparing` via a hook, not a real backend Prepare failure |

## Measured result (against `6ddb2bd`, `git diff --numstat HEAD`, `.go` files)

| | added | removed | net |
|---|---|---|---|
| production | 33 | 1756 | **-1723** |
| tests | 598 | 2879 | **-2281** |

Of the test additions, 285 lines are relocated fixtures. Production removal is
close to the candidate document's ~1,735 gross estimate. Documentation changed
separately: `docs/vmbot.md` is now a historical pointer (the old guide is in git
history), `architecture.md` states the real `cmd/vmbot` dependencies,
`control-api.md` and `dbos-orchestration.md` no longer claim live legacy
controller paths. ADRs and verification evidence are untouched.

## Verification run

* Baseline before edits: `GOMAXPROCS=2 GOFLAGS=-count=1 go test -timeout 300s
  ./internal/vmbot ./internal/controlapi ./cmd/vmapi ./cmd/vmbot ./internal/tgbot`
  all `ok`.
* After: full `GOMAXPROCS=2 GOFLAGS=-count=1 make check` run serially, exit 0
  (race tests with `-timeout 3m`, Python suites, vet, shell syntax, control-build,
  no-grpc, consumer-smoke, verify-units). Package results: `internal/vmbot` ok
  85 s (83.8% statements), `internal/controlapi` ok (88.3%), `cmd/vmapi` ok
  (73.1%), `cmd/vmbot` ok (59.6%), `internal/tgbot` ok (75.7%).
* `CGO_ENABLED=0 go build` of `vmapi`, `vmbot`, `vmctl` into a scratch directory
  succeeded.

## Limitations

* No real-guest or live-service run of the cleaned owner was performed; that
  remains a separate step before any activation.
* The new test injects failures by wrapping the `Lifecycle` (`hookLC`) and
  overriding the reported VM state; it does not drive a real backend Prepare/
  Start failure. Control-flow only; no live Firecracker. The shared fake terminal
  records Start/Revoke calls, so "no terminal grant" is asserted for the retained
  VM only (other VMs' creates are not required to grant none).
* Several removed tests (for example ambiguous Prepare keeps the slot, NotFound
  during pending Create, delete barrier) exercised vmlib safety guarantees
  through the old controller. The guarantees live in `vmlib`/the owner path, and
  owner tests cover the active equivalents, but a one-to-one mapping of every
  removed assertion was not made; check `vmlib` tests if that matters.
* The non-control `Store` mode, its terminal-authorization branch and the
  legacy JSON fields remain by design and are not exercised end to end by any
  shipped binary other than through migration.
* Line counts are physical (comments/blanks included) and relative to the
  baseline commit, not a quality metric.

## Parent verification and independent review

Independent source review found no blocking logic/security issues; a separate
removed-test audit prompted the retained-quota owner regression and stronger
process-delete/retirement assertions documented above. Production code did not
change during that test-only follow-up.

Parent ran full `GOMAXPROCS=2 GOFLAGS=-count=1 make check` uncached and built
`vmapi`, `vmbot`, `vmctl` into an external scratch directory: exit 0.
After the test-only follow-up, parent also ran
`GOMAXPROCS=2 go test -race -count=1 -timeout=180s -run
'TestOwnerAmbiguousCreateRetainedQuota|TestControlRealKillRecovery|TestOrchestrationRecoversInterrupted'
./internal/vmbot`: exit 0. `git diff HEAD --check` was clean.

The live service remains at the pre-cleanup verified revision. No real guest run
or activation of this cleanup is claimed.
