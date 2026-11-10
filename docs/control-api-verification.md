# Control API verification

The `feat/control-api` source was verified on 2026-11-10. It is not installed as
the production control API or HTTP-only bot. Deployment requires a coordinated
state/offset handoff; do not run the legacy bot and API against the same state.
See [the migration guide](control-api.md) and [frontend guide](bot-api-client.md).

## Executed final integration gates

Parent reran, after the reviewed fixes and capability forwarding:

- `GOMAXPROCS=2 make check`: PASS. Includes race+coverage Go tests, 38 Python
  tests, vet, actual vmapi/vmbot builds, real protobuf regeneration comparison,
  external-module consumer, portable client cross-builds and rendered systemd
  unit verification. Unit verification does not install or activate services.
- `GOMAXPROCS=2 make control-smoke`: PASS, uncached.
- `GOMAXPROCS=2 go test -race -count=1 -timeout 3m ./internal/controlapi
  ./internal/vmbot ./controlclient ./internal/tgbot ./cmd/vmapi ./vmlib/...`:
  PASS. Explicit uncached run; core domain took approximately 85 seconds.
- Original three review reproductions rerun through a scratch Go overlay under
  `-race -count=3`: PASS after the fixes (each failed before its fix).
- `git diff --check`: PASS.
- `docs/control-api-openapi.yaml` parsed and validated as OpenAPI 3.0.3 with
  `openapi-spec-validator` in an isolated temporary Python environment. It has
  11 path entries. This validates document structure, not every runtime payload.

Selected statement coverage from `make check`: control HTTP 78.0%, shared domain
83.2%, Go control client 89.3%, Telegram adapter 70.4%, local backend 78.1%, remote
transport 83.8%. The vmapi main package's reported coverage is 9.5% because the
executable smoke runs its separately built child process without collecting that
child's coverage. Passing tests/coverage are not proof of all possible races.

## Executable smoke boundary

`cmd/vmapi/TestExecutableControlFlow` runs the actual API `run()` entry point
in-process, injects the `vmlibtest` backend through the test-only opener seam and
drives its HTTP API using the public Go client. This is real run()/HTTP/TLS/DBOS/SQLite wiring, but the
VMM and guest resources are fakes: no KVM or production filesystem is involved.

It checks:

- health, invalid bearer and forbidden delegated identity;
- explicit CPU/RAM creation, polling and persisted resource view;
- same-key replay, changed-body conflict and foreign VM/image 404s;
- owner quota and committed source occupancy;
- SSH metadata and explicit rejection of unsupported caller-key provisioning;
- cold commit into a private image, source deletion, derivative creation,
  reference-protected image deletion, final cleanup and shared-base protection;
- graceful process termination, reopening the same state, retained operation
  results, released lifetime lock and absence of credentials in logs.

The smoke does not enable terminal configuration. Live guest SSH/ttyd/browser
operation is not re-attested for the new split API architecture. Terminal
revocation, projections and ownership fences are covered by package tests with
fake subprocesses. Earlier real-host DBOS-bot evidence is separate and must not
be treated as deployment verification of these new components.

## Review-driven regressions

- Publish refuses an unresolved existing classification marker and never erases
  its prior evidence on failure. Exclusive marker creation and fail-closed
  destination inspection were added after both orphan-marker cases failed.
- A transient private-image deletion failure keeps the image owner-visible in
  `deleting` phase. Original key still returns its original failed operation;
  a fresh key retries only after a terminal attempt. Restart and already-absent
  remote image are tested; deleting images remain unbootable.
- Legacy persisted deleting intent is atomically assigned an owner-scoped stable
  delete operation during migration; startup repairs an earlier control state
  lacking that operation. Identity/generation/sequence/capability/offset are
  preserved, and repeated recovery does not duplicate the intent.
- Stop, commit and terminal HTTP replay returns the original operation even
  after VM removal. A different owner cannot replay it; a fresh request still
  returns 404; changed commit input returns 409.
- Capabilities are actual backend profile/resource policy read in-process from
  `vmlib.Service`. API user/global quotas remain distinct from backend aggregate limits;
  HTTP response/Go-decoder tests cover the HTTP half.

## Operational limits

- Ambiguous commit publication cannot be safely adopted without backend source
  provenance. It remains private/unbootable, retains the source and requires
  operator reconciliation. This is deliberately fail-closed, not transparent
  exactly-once image publication.
- Cold commit stops the source; no live-memory snapshots, arbitrary disk resize
  or automatic PostgreSQL tuning are promised.
- Requested CPU/RAM must fit backend policy and installed authority. Catalog
  capabilities are not a reservation or a promise of current free capacity.
- The local backend supports preinstalled SSH authentication only; caller-key
  injection is explicitly rejected, never silently ignored.
- Failed VM mutations retain occupancy until safe explicit cleanup. Startup
  repair is one-shot; degraded operation recovery remains observable through
  the API and a restart re-runs repair. No TTL, automatic history pruning or
  rate-limiting feature was added.
- Legacy queued-delete handoff tests exercise the retirement path in the new
  engine, not an old production binary creating the SQLite row. Actual live
  migration, Telegram token/offset handoff, host resource authority and external
  reachability need deployment acceptance.
- Bearer terminal links can be forwarded. API owner isolation is not guest
  network isolation. No external API listener, credentials, host service,
  firewall or production VM was changed during this source verification.
