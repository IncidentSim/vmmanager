# API policy contract and integration

Create is `{name?, image_id, profile?, ttl_seconds?, ssh_public_key?}`. Public
`resources` input is removed (422 `validation_failed`, field `resources`). Use
`GET /v1/capabilities` for profiles/default, quotas and actual support. SDK
`CreateRequest.Profile` / `TTLSeconds`, CLI `create -image IMG -profile small
-ttl-seconds 3600`, bot `/create [name] [image_id] [profile] [ttl_seconds]` share
this contract. Bot config uses `profile` and optional `vm_ttl_seconds`; old
`resources` configuration is rejected, not silently dropped.

## Ready integration seams

Before serving, call `Server.Configure(Options{Policy: cfg.PublicPolicy(), ...})`.
`Config.PublicPolicy()` carries configured defaults but all feature flags are
false. Set `TTL`, `SuppliedSSHKey`, `GeneratedSSHKey` only after implementing the
corresponding owner/backend behavior. `TTL` materializes the default and passes
`ControlRequest.TTLSeconds`; supplied keys are genuinely parsed, stripped of
comments and passed in `SSHPublicKey`. This worktree's domain currently refuses
nonzero TTL/supplied key: lower-layer integration is required, not fake success.

`KeyProvider.GeneratedKey(ctx, ownerID, vmID) (GeneratedKey,error)` must load
stable credentials from encrypted storage, outside DBOS. HTTP checks ownership
before provider invocation and responds no-store on all endpoint outcomes.
Map supplied-key cases to `ErrKeyNotGenerated` (409); other provider failures
become sanitized 503. `Options.ErrorMapper` maps domain sentinels (e.g. expired)
to HTTP status/code without echoing error contents. Configure is startup-only.

The domain remains responsible for immutable effective resources, admission
idempotency/digest, persisted deadline/expiry and actual offline SSH provisioning.
No generated private key enters Create DTOs, domain history or logs here. The
SDK explicit `SSHKey` method returns sensitive data; callers must not log it.

## Upgrade-safe intent and disk reservations

Create retries first perform an authenticated, owner/key-scoped, read-only semantic
digest lookup. A frozen pre-profile HTTP DTO reproduces the old digest (including
explicit legacy CPU/RAM resources). It can only return an existing association:
new requests containing `resources` still receive field validation errors, and a
changed legacy payload conflicts rather than acquiring admission rights. Current
HTTP intent has a versioned digest, preserves omitted profile/TTL independently
of their effective defaults, and canonicalizes supplied keys. Versioning prevents
current key normalization from accidentally aliasing changed legacy raw keys.
Neither replay path resolves the public profile catalogue or invokes effects.

Admission records the selected profile's `DiskMiB` separately from public
CPU/RAM resources. The owner carries this immutable snapshot through
`vmlib.CreateRequest` and `PrepareRequest` into persisted local metadata. A
nonzero trusted disk snapshot accompanies explicit effective CPU/RAM, survives
catalogue edits/removal, and does not bypass aggregate host budgets. Zero retains
the historical explicit-resource smallest-profile disk bound and profile-maxima
checks; old reservations are not silently expanded during migration. No DBOS
step sequence changes are needed.

Regression coverage: `TestOldHTTPStateUpgradeReplay`,
`TestOmittedProfileIntentSurvivesDefaultChange`, and
`TestOwnerSelectedDiskSnapshotSurvivesRestart`. The disk test exercises the real
owner/facade/local admission path with a sparse 6000 MiB image, 4096/8192 MiB
profiles and fake VMM/launcher, not a real guest.

## Documentation and tests

`/openapi.yaml` inserts its CreateVM example from configured shared image and
default profile (plus effective default TTL only when supported). With no shared
image no runnable example is invented. The configured image must actually be a
bootable/provisionable base: this cannot be inferred from its ID. Swagger keeps
retry identity in page memory for the same principal/method/URL/body; explicit
keys are preserved. To intentionally create an identical new intent, enter a
new key or reload the page. No credential persistence/localStorage is used.

`TestPublishedSwaggerExampleCreatesAndReplays` executes the published example
through real HTTP and DBOS SQLite admission with a fake VM backend. Live
Firecracker SSH/provisioning/CPUQuota/expiry verification remains an integration
gate; fake tests are not evidence of those host effects. Existing vmapi process
smoke now sends a profile instead of removed resource overrides.

New policy defaults are max_vms=4, max_vms_per_user=2, default/max TTL=3600/86400.
An omitted API max_vms is bounded by an explicitly smaller backend max_vms;
this does not increase physical slots or activate services. Explicit oversized
quotas remain configuration errors. Feature flags stay false in cmd/vmapi until
parent wires real lower-layer behavior.
