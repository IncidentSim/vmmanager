# VM owner workflow: verification report

Implements [docs/rfc-vm-owner.md](rfc-vm-owner.md). Source verified in this worktree
only; **nothing was deployed, activated, or run against production state, services,
networking, VMs or credentials, and no live Firecracker evidence exists.** Every
"real" below means: real controller/`vmapi` `run()`, real DBOS v1.8.0 on a file
SQLite database, real HTTP and public client, and the in-memory `vmlibtest`
lifecycle fake (or a file-backed fake for process-kill tests). A real
`SIGKILL` of a test subprocess is real process death, but the fake effects it
interrupts are in-process code, so it does not model an independently delayed
launcher/backend, power loss, or Firecracker itself.

Design notes: [architecture.md](architecture.md#vm-owner-workflow). API notes:
[control-api.md](control-api.md). Isolated spike used as reference only:
`dbos-vm-owner-spike` (outside this repository; nothing was copied wholesale).

## Baseline and test-first

* Baseline before any change: `go build ./...`, `go vet ./...` and
  `go test ./...` all passed (including `internal/vmbot` ~24 s non-race).
* Contract tests (`internal/vmbot/owner_contract_test.go`) were written first and
  run against the old code: 7 failed on behavior (no owner workflow, admission
  mutated the phase to `deleting`, unbounded second Delete, command for a dead
  owner accepted, no settled-record adoption, no startup rejection of pending
  legacy state); the invariants that already held (independent VMs, duplicate /
  changed-payload handling) passed. All pass on the implementation.

## Commands executed (final tree)

| Command | Result |
| --- | --- |
| `GOMAXPROCS=2 make check` (race+cover `go test ./...` with 3 min per-package timeout, 41 Python tests, `go vet`, `go build ./cmd/vmapi ./cmd/vmbot`, no-grpc check, external-module consumer smoke + darwin/windows cross-builds, rendered-unit `systemd-analyze verify`) | exit 0 |
| `go test -race -count=1 ./internal/vmbot` inside the preceding `make check` runs | pass, 117-134 s uncached (limit 180 s; the last `make check` reused that identical-source result from the cache) |
| `GOMAXPROCS=2 go test -race -count=1 -run 'TestOwner\|TestControlRealKillRecovery\|TestControlChildFailure\|TestControlDelete\|TestControlStop\|TestControlMigration\|TestControlRecover' ./internal/vmbot` | pass, 50.7 s |
| `GOMAXPROCS=2 go test -race -count=1 ./cmd/vmapi ./controlclient ./internal/controlapi` | pass |
| `go test -race -count=3 -run 'TestOwner\|TestControl\|TestCommon\|TestProtected\|TestPending' ./internal/vmbot` | every test passed 3x with no data race except `TestOwnerLegacyVersionWorkflowIsNotRecovered`, whose fixture counter was not reset between iterations (test-only defect); fixed, then that test plus the pinned-name and graceful-shutdown tests re-run with `-race -count=3`: pass |
| `GOMAXPROCS=2 make control-smoke` | pass |
| `CGO_ENABLED=0 go build -o /tmp/vmo-bin/{vmapi,vmbot,vmctl}` (outside the repo) | built |
| `git diff --check` | clean |

One unrelated Python test (`test_verify_image.VerifierSignalTests`, a 10 s signal
timeout in the image verifier) failed once under load while binaries were being
built concurrently; it passes alone and in the final `make check`. It does not
touch this change.

## What each test proves (acceptance criteria)

All in `internal/vmbot` unless noted; fake lifecycle, real DBOS SQLite.

1. **Lifecycle through the real controller** - `TestOwnerFullLifecycle` (create,
   terminal, stop, two repeated cold commits producing two independent images,
   delete; owner `PENDING` between commands and `SUCCESS` only after delete).
   `cmd/vmapi` `TestExecutableControlFlow` drives the same through real `vmapi`,
   HTTP and `controlclient` polling; `TestOwnerSurvivesAPIRestart` restarts the
   whole API on the same state with a live VM (no second backend Prepare).
2. **Sequential, independent, deferred Delete** - `TestOwnerDeleteWaitsForRunningChild`
   (Delete delivered while Stop is blocked: phase not `deleting`, no extra terminal
   revoke, no backend delete, workspace still present until Stop settles, then Stop
   precedes Delete), `TestOwnerBacklogIsBounded`, `TestOwnerTwoVMsIndependent`,
   `TestOwnerRaceNoOverlapAndBoundedBacklog` (-race; ~14 admitted operations among
   thousands of contending attempts, zero same-VM effect overlap) with
   `TestOwnerOverlapDetectorSeesOverlap` as positive control,
   `TestControlDeleteWaitsForQueuedCommit` (the earlier Commit now publishes first).
3. **Idempotency** - `TestOwnerDuplicateAndChangedPayload` (duplicate attaches; a
   changed payload under the same key is 409 before any effect, also after VM
   deletion; completed duplicate returns the original), `TestOwnerRaceConcurrentDuplicatesOneEffect`
   (16 concurrent duplicates, one backend stop), `TestOwnerRaceQuota`,
   `TestControlDurableAdmission`. A command for an ended owner is refused
   (`ErrOwnerUnavailable`, HTTP 503), never reported accepted.
4. **Process kill/restart** (`TestControlRealKillRecovery`, real `SIGKILL` of a
   subprocess, same database and file-backed state): `enqueue-gap`,
   `create-effect`, `delete-intent`, `delete-effect` (pre-existing modes, now
   through owners), plus new `idle-owner` (owner survives and takes a new
   command, no second create), `stop-effect` (effect done before checkpoint: raw
   effect ran 1-2 times, outcome recorded once), `stop-before-projection` (child
   complete, process killed before projection: effect ran exactly once, outcome
   recovered by replay). Raw effect invocations are **not** exactly-once; the
   backend's stable-ID idempotency/barriers carry safety. `TestOwnerGracefulShutdownDuringChildResumes`
   covers graceful shutdown mid-child (found and fixed a real defect: the interrupted
   wait was recorded as a failed outcome; the owner now returns the cancellation
   unrecorded as the SDK requires).
5. **Failure handling** - `TestOwnerExpectedFailureLeavesOwnerUsable`,
   `TestControlStopCanRetryFailedAttempt`, `TestControlChildFailureIsOutcomeNotRepair`
   (a panicking child before/after the VM exists is an outcome, record retained,
   no recreate, explicit Delete settles), `TestOwnerTerminalErrorVisibleAndRetained`
   (cancelled owner) and `TestOwnerUnexpectedErrorIsVisibleAndRetained` (real
   workflow `ERROR`): refused commands, VM and quota retained, `owner_error`,
   recovery reports it, exactly one owner workflow (no replacement).
6. **Existing state / upgrades** - `TestOwnerAdoptsSettledRecordWithoutCreate`,
   `TestOwnerRejectsPendingLegacyCreate`, `TestControlMigrationLegacyCreatingNeverIssuesCreate`
   (startup rejected, diagnostic names VM/operation, backend untouched, state kept),
   `TestControlMigrationSynthesizesDeleteForLegacyIntentBeforeEnqueue`,
   `TestControlRecoverRepairsDeletingVMWithoutOperation`,
   `TestControlMigrationQueuedLegacyDeleteWorkflowHandoff`,
   `TestOwnerLegacyVersionWorkflowIsNotRecovered` (a workflow left `PENDING` under
   the old application version stays untouched and unexecuted),
   `TestOwnerPinnedWorkflowNamesAndVersion` (pins names/version/topic/ID scheme).
   **Not exercised here:** `dbos.Patch` evolution of a live owner. The policy is
   documented; the only evidence is the spike's, which is not reproduced in this
   tree.
7. **Regression coverage kept** - authorization, quota, common images, no-overwrite
   commit ambiguity (`TestControlCommitAmbiguityFailsClosedAndNeverAdopts`),
   clean-exit and terminal-fence tests all pass unchanged in assertions
   (`internal/vmbot`, `internal/controlapi`, `vmlib/...`).

## Existing tests changed, and why

Only for the agreed sequential semantics or because the removed execution API no
longer exists. No assertion about authorization, quota, images, retention or
safety was loosened.

* `internal/controlapi/http_test.go` `TestAuthenticatedHTTPContract`: was "stop
  must fence terminal before enqueue"; now the terminal is fenced once the Stop
  executed (no admission-time revocation).
* `control_order_test.go`: `TestControlDeleteSupersedesQueuedCommit` became
  `TestControlDeleteWaitsForQueuedCommit` (Delete no longer supersedes; the Commit
  publishes first). The stop-retry test now injects a real backend failure
  instead of editing the store.
* `control_repair_test.go`: the old test proved a restart-time *repair* of a
  failed workflow; replaced by `TestControlChildFailureIsOutcomeNotRepair`
  (outcome + retention, no repair).
* `control_legacy_delete_test.go`: `...LegacyCreatingNeverIssuesCreate` was
  "reconciled through Get at recovery"; it is now an explicit startup rejection
  (still asserts no Create and no backend change).
* `control_common_images_test.go`, `control_common_policy_test.go`,
  `control_image_retry_test.go`, `control_projection_test.go`: the synchronous
  `executeControl` driver is gone, so rigs start a real controller and await
  operations; "pending but not yet executed" scenarios use a gated backend call
  or a stopped controller re-delivered at start. Their assertions are unchanged.
* `control_process_test.go`, `orch_process_test.go`: three new kill modes, a
  `Stop` on the file-backed fake and a nil-in-production `testBeforeProject` seam.
* `t.Parallel()` was added to independent owner-based tests (each has its own
  state directory and DBOS database) to keep the race suite well inside the 3 min
  Makefile timeout.

## Old active orchestration actually removed

In `internal/vmbot/control.go` / `orch.go` (`owner.go` is new; exact source-line
accounting appears below):

* `executeControl`: the per-operation body that decided phases and wrote final
  state (create/stop/commit/delete/terminal/refresh/delete_image branches).
* `controlFlow`, `submitControl` and the `"control"` workflow kind on the
  partitioned `vmops` queue (control operations no longer use a queue at all).
* Admission-time lifecycle mutation: setting `stopping`/`committing`/`deleting`
  phases and bumping `Seq` in `AcceptControl`, and `c.term.Revoke` at admission.
* The old `RecoverControl`: workflow-status inspection, attempt-counter resubmission
  with new workflow IDs, the `Repair` flag and its absence-barrier repair, and the
  per-VM `refresh` operations it created at every start (one durable operation per
  VM per start).
* `OpInput.OperationID`. (`Attempt`/`Repair` stay as ignored fields only so strict
  state loading still accepts upgraded files.)

Not removed (honest scope): the single-tenant, non-control Telegram
`Controller` path (`createFlow`/`deleteFlow`/`terminalFlow`/`reconcileFlow`, `Bot`,
their `vmops` queue and their tests) is not used by `cmd/vmapi` or
`cmd/vmbot`, but it is not replaced by the owner either and is covered by its own
tests; deleting it is a separate cleanup. The application-version bump to
`vmbot-owner-1` also means workflows left `PENDING` by an earlier build on that
legacy path would no longer be recovered by this build.

## Remaining authoritative state writers

* Admission (`AcceptControl`, short atomic store update): **new** VM identity
  (`creating`), operation records (`pending`), image reservations (`publishing`),
  the `deleting` fence for shared-image deletion, plus failing an operation
  whose owner is provably dead.
* VM owner (`beginOp`/`applyOutcome` steps): VM `phase`, `state`, resources, VM
  removal, operation `running`/`succeeded`/`failed`, image `ready`.
* `imageDelete` workflow: its own operation and image removal.
* Startup (`checkHandoff`, `RecoverControl`, `OpenControlStore`): legacy
  delete synthesis, adopting/abandoning pre-owner operations.
* Children (`vmEffect`) never write the store. Terminal processes are local
  projections written only through `Terminals`.

## Known limitations

* **No immediate revocation, no preemption**: access and the VM can stay available
  until the owner reaches the command; a long Commit delays Delete. By design.
* **Latency**: DBOS v1.8.0 polls its mailbox every second (not configurable), so
  each command costs up to ~1 s; the `internal/vmbot` race suite grew from tens
  of seconds to ~2 min.
* **Terminal owner ERROR/cancel is retained, not repaired**; no replacement
  owner, poison-history repair or history rollover. Each idle period (6 h) adds one
  history row to a long-lived owner. A command admitted just before an owner ends
  stays `pending` (reported `failed`/`unavailable` on read) because delivery is
  not transactional with owner status.
* Failed Stop/Commit/Delete keep their transitional phase (access fenced,
  capacity retained) until an explicit follow-up; a failed Commit leaves the
  private image `publishing` and the source `committing` (manual recovery as
  before).
* A restart during a long effect depends on DBOS's 10 s graceful-shutdown wait;
  the backend call itself is detached from cancellation and is not stopped by the
  owner. A timed-out child does not prove the effect ended; backend per-VM
  serialization and the settled-delete barrier are what guard a later Delete.
* Pre-owner handoff is deliberately narrow: VMs left `creating`/`stopping`/
  `committing` by an earlier build make startup fail with a diagnostic and need
  operator action. Rollback after owners exist is unsupported.
* `dbos.Patch` compatibility is documented, not exercised here. Process-kill
  tests use fakes; live Firecracker, host helper/systemd, real ttyd/ssh and
  authenticated SSH are not covered.
* The SDK logs a WARN per owner at shutdown ("Recv() context cancelled"); it is
  noise, not an error.

## Review fixes (focused correctness pass)

Regressions were written first (`internal/vmbot/owner_review_fixes_test.go`) and
run RED against the unfixed tree (evidence kept in the scratch dir as
`review-red.txt`):

```
--- FAIL: TestReviewBeginIsIdempotentForCurrentOperation: commit begin #2: run=false note="rejected: conflict"
--- FAIL: TestReviewTerminalOnStoppedIsImmediateConflict: terminal on stopped: <nil>
--- FAIL: TestReviewDeleteSettlesEarlierUndeliveredCommands: earlier stop left "pending" after owner success
--- FAIL: TestReviewPreparedWorkspaceIsNotSuccessfulCreate: ... Status:succeeded
--- FAIL: TestReviewSparseLegacyRecordGetsProjection: timeout: projection populated
```

Fixes (all in `owner.go`, plus one admission check in `control.go`):

1. **Begin idempotent.** `beginOp` accepts a `commit` whose op is already
   `running` and whose VM is already `committing` (the store write that precedes
   the lost DBOS checkpoint). A merely `pending` op at that phase is still
   rejected, so a stale Commit cannot begin. Stop/Delete/Create/Terminal and
   `imageDelete` were audited: their begin writes were already replay-safe;
   `applyOutcome` is one atomic update and repeats harmlessly. The test calls
   `beginOp` twice (the deterministic lost-checkpoint boundary).
2. **Prepared workspace is not a successful Create.** `runEffect` create requires
   the observed VM to be `running`; a `preparing` Get (or a Create result that is
   not running) returns `unavailable` with Exists/State and no Create call.
   `applyOutcome` promotes to `active` after a failure only when the observed
   state is `running`. The record stays `creating` (identity, image, capacity
   retained) and an explicit Delete succeeds. Conservative failure, no resume.
3. **Admission vs mailbox order.** Before running a `delete`, the owner runs a
   checkpointed step `prior` listing the VM's unfinished earlier commands in
   stored admission order and handles each (same begin/child/project path) first.
   Their later messages find them finished. Admission already bounds this to one
   command. Test: the earlier Stop/Commit's delivery fails (orchestration
   unavailable), orchestration returns, Delete is admitted: after owner success
   the earlier op is `succeeded`, ran exactly once before Delete, no image stays
   `publishing`. `TestReviewLateSendWhileDeleteBlocked` also sends the earlier
   command through the actual `deliver` path while Delete is blocked, then
   restarts recovery: neither Stop nor Commit repeats, and their outcomes remain
   succeeded. No new `Send` locking, production test seam or dispatcher.
   Note: `prior` is a new durable step in the delete path; owners with a Delete
   in flight from a build without it would need `dbos.Patch` (this tree is
   unreleased).
4. **Sparse legacy bootstrap.** `rebuildTerminal` (owner-run, once per process
   per VM, retried if the backend Get fails transiently) fills only EMPTY
   `ImageID`/`Resources`/`RemoteState` from the observed VM for settled phases.
   Token, name, owner and phase are untouched; a deleted record is not
   resurrected (`update` is a no-op for a missing VM). No Create is called.
5. **Terminal on stopped/committed** is an immediate `ErrControlConflict` at
   admission again (independent of deferred Delete semantics).

Tests run (all pass after the fixes, `GOMAXPROCS=2 -count=1`):

* `go test ./internal/vmbot/ ./internal/controlapi/ ./cmd/vmapi/ ./controlclient/`:
  all `ok` (vmbot 63s).
* `go test -race -run 'TestReview|TestOwner' ./internal/vmbot/`: `ok` (25s).
* `go vet ./internal/vmbot/`: clean. Not run: whole-repo `make check`.

Code accounting (honest): `git diff HEAD --numstat` for active, non-test Go files
is control.go +230/-251, orch.go +25/-12, controller.go +7, client.go +3, plus
the new `owner.go` (624 lines, 71 of them added by this pass). Net active code is
**+626 non-test Go source lines (including comments/blank lines), an increase**,
not a reduction. What was removed is
mechanism (per-operation legacy workflows, attempt/repair state machine, dispatch
retry loops; see "Old active orchestration actually removed"), not line count.

Remaining blockers: none known for these five items; still unverified against
live Firecracker. Reduction in total source-line count was not achieved.

## Independent final verification

After the focused fixes, an independent source review found no concrete logic or
security issues. It also ran an actual delayed-Send regression for both Stop and
Commit, normally and under `-race`. That regression is retained in
`owner_review_fixes_test.go`; the earlier no-op claim that `RecoverControl` after
deleting a VM tested late delivery was removed.

Parent-owned execution, after production-code fixes:

* `GOMAXPROCS=2 GOFLAGS=-count=1 make check`: exit 0. All Go packages passed
  `-race -cover`, all 41 Python tests passed, `go vet ./...`, build, no-grpc,
  consumer/cross-platform smoke and rendered-unit verification passed.
* `GOMAXPROCS=2 make vmapi vmbot vmctl BUILD_DIR=$TMPDIR/vm-owner-build`:
  exit 0, all three static binaries produced outside the repository.
* After retaining the independent late-Send regression:
  `GOMAXPROCS=2 go test -race -count=3 -timeout=180s -run '^TestReview' ./internal/vmbot`:
  exit 0. Production sources were unchanged by this test-only follow-up.
* Added-line/new-source secret/injection/deserialization scan: no matches;
  `git diff --check`: clean.

Live Firecracker and activation were deliberately excluded; these results are
not live VMM, SSH or deployment acceptance. The previous live service is untouched.
