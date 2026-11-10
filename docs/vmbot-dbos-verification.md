# Multi-VM DBOS verification

Verified on 2026-11-10. This report covers the **operation-per-workflow** design
in ADR 0004, not a long-lived workflow owning the entire VM lifecycle.

## Repository checks

- `make check`: PASS, including `go test -race -cover -timeout 3m ./...`, Python
  tests, vet, shell syntax, protobuf regeneration check, separate-module public
  consumer, portable client cross-builds and rendered systemd unit verification.
- `internal/vmbot` statement coverage in the final complete run: **84.0%**.
- `make vmbot BUILD_DIR=.tools/dbos-build`: PASS, `CGO_ENABLED=0`.
- `git diff --check`: PASS.
- Existing adversarial terminal-manager overlay: all four regressions PASS
  (concurrent revoke/close, failed-start cleanup, replacement isolation and
  pending-token uniqueness).

## Durable orchestration tests

Normal package tests use real DBOS v1.8.0 and SQLite files, with fake remote
resources kept separately from the database:

- Actual child-process `SIGKILL` after external Create and before checkpoint:
  restart adopts the same resource, does not repeat Create and rebuilds its
  terminal projection.
- `SIGKILL` with durable delete intent but no workflow submission: startup
  re-submits the intent and completes deletion.
- `SIGKILL` after external Delete and before checkpoint: replay converges to
  absence.
- Delete intent while an uncheckpointed Create is in flight: recovery does not
  reauthorize the terminal and converges to deletion.
- One engine per Store; a second process is denied the state-directory lock;
  Store cannot release its lock while the engine owns it; Start on a closed
  Store fails.
- Duplicate active Delete commands attach to one attempt; a reused workflow ID
  with different input is refused without touching either VM.
- Slow terminal cleanup for one VM does not block another VM's deletion.
- SDK terminal ERROR both before and after the external Create effect: recovery
  uses fresh guarded reconciliation, never a fresh Create. An existing VM keeps
  its identity/capability and regains terminal access; an absent one releases
  occupancy only through the successful Delete barrier. These cases, the real
  kill tests and cross-VM cleanup were also repeated three times under `-race`.
- Deterministic terminal authorization interleavings reproduced HTTP 200 before
  the fix. Both now return 404: late startup after durable delete/revoke and new
  requests during the interval between durable delete intent and local cleanup.

The initial review identified the terminal-ERROR/Busy defect; the permanent
regression was executed failing before its fix and passing afterward. A passing
race detector alone is not the basis for the ordering claims above.

## Real-host smoke test

The previously deployed bot and facade had **no VM records/resources** at
preflight. No earlier user VM was recreated or adopted. The bot was stopped for
exclusive state ownership; its binary, configuration and state were backed up
privately before testing. The live harness never read or polled the Telegram
bot token and guarded all existing identities against destructive operations.

Using the actual facade, Firecracker backend, real image, ttyd and HTTPS gateway:

1. v1 empty state migrated to v2; DBOS SQLite started in the actual private state
   directory.
2. Two disposable VMs were created and passed SSH readiness.
3. A third Create was refused by application capacity.
4. Both real HTTPS pages returned 200 repeatedly. Bad capability returned 404,
   URL command arguments returned 400 and hostile WebSocket Origin returned 403.
5. Both real ttyd WebSockets upgraded and executed guest commands. Success
   markers were assembled by the guest command so echoed input alone could not
   satisfy the assertion.
6. Deleting VM B actively closed its existing WebSocket and made its URL return
   404. VM A's page stayed 200 and its already-open WebSocket executed a new
   guest command afterward.
7. The harness finished with exit 0 and deleted its remaining disposable VM.
   Read-back through the facade and application state confirmed both were empty.

## Deployment and rollback boundary

The feature was **not activated as the production bot**. The unchanged prior
single-VM binary was restarted for review/merge. Because no VMs remained and the
Telegram offset was unchanged, empty v2 state was converted back to v1 while
preserving the current generation high-water mark and offset. This was not a
blind restoration of stale v1 bytes. The tested v2 state and SQLite files were
retained in the private backup. Service read-back confirmed the prior bot and
facade active.

No TLS/firewall/host-unit changes or VM capacity-policy increases were made.
Capability URLs, credentials, private keys and filled runtime state are not
part of this report or Git changes.

## Limits

- Real-host smoke used the harness through the production controller/manager and
  facade, not Telegram command delivery through the new bot executable.
- The real-host test did not kill a VMM or crash the production daemon. Durable
  crash/recovery was exercised with separate child processes and fake remote
  resources, not a claim of exhaustive host-failure testing.
- External effects still need stable identities/reconciliation. Workflows do
  not persist live SSH/WebSocket connections, and queue concurrency is not FIFO.
- Long-term SQLite growth, recovery-budget exhaustion, cross-version workflow
  migration and disk-full behavior remain operational follow-up work.
