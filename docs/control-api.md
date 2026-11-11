# Control API (vmapi) v1

`cmd/vmapi` is the standalone authenticated HTTP control plane. It is the only
owner of application state: users/quota, VM and image ownership, DBOS workflows,
the state-directory lifetime lock, and terminal projections. The Telegram
frontend (`cmd/vmbot`, see `bot-api-client.md`) is a separate HTTP-only client
and has **no** direct VM fallback: if the API is down, the bot reports
an error and does nothing else. The API embeds `vmlib.Service` over
`vmlib/local.Backend` in-process (Firecracker via the host helper); there is no
VM gRPC plane or `vmd`. Backend additions and the private `backend` policy are in
`control-api-backend.md`. Rationale: `adr/0005-control-api.md`. Machine-readable
schema: `control-api-openapi.yaml`.

Nothing here installs or starts a service. `deployment/vmapi/` contains source
templates only.

## Configuration (`deployment/vmapi/vmapi.example.json`)

Strict JSON (unknown/duplicate-trailing data rejected, max 128 KiB); one flag,
`-config <path>`. No secret is ever accepted on the command line.

| Field | Meaning |
| --- | --- |
| `listen_addr` | default `127.0.0.1:8090`. Plaintext HTTP **must** bind a loopback IP; anything else requires `tls_cert_file` + `tls_key_file` (both or neither). |
| `state_dir` | absolute; holds `state.json`, `lock`, the DBOS SQLite files and migration backups. |
| `allowed_user_ids` | required, unique positive numeric IDs. This is the only user/authorization list; removing an ID denies the user but never deletes or reassigns resources and never frees quota occupancy. |
| `legacy_owner_user_id` | required only to adopt a non-empty legacy v1/v2 state (see Migration). An empty state starts without it. No "first allowed user" guess and no owner 0. |
| `shared_image_ids` | explicitly configured base images visible to everyone and never deletable via the API. The backend catalog is **not** implicitly public. |
| `max_vms` (default 2), `max_vms_per_user` (default 1) | positive quotas. The backend capacity stays the final authority. |
| `default_profile` (default `small`) | used when a Create omits `resources`. |
| `credentials[]` | each `{"token_file":path}` (0600 regular file, ≤4 KiB, one token, no whitespace) or `{"sha256":hex}`; plus either `"user_id":N` (ordinary user bearer) or `"service":true` (delegated bot connector). Digests are compared in constant time; tokens are never logged. |
| `backend` | required private local-backend policy: exactly the object formerly at `vmd` `backend.local` (roots, helper, profiles, aggregate caps, storage mount, guest pool, bastion). A leftover `facade` section is rejected. Mapping and handoff: `control-api-backend.md`. |
| `terminal.*` | optional. Empty `public_origin` disables terminals (Terminal operations then fail `unavailable`). Plaintext listener only on loopback; `max_clients*max_vms ≤ 64`. |

## Authentication and authorization

* `GET /healthz` and the read-only documentation routes (`/docs`,
  `/docs/assets/*`, `/openapi.yaml`, a self-hosted Swagger UI) are the only
  unauthenticated routes. Health returns `{"status":"ok"}` or
  `{"status":"degraded"}` and nothing else (see Startup repair).
* **JWT user tokens** (config `jwt`, `allowed_usernames`): an RS256 bearer signed
  by the bot and verified with the public key only; `sub` is the numeric owner,
  the signed `tg_username` claim may bootstrap a configured username slot which
  is pinned durably to that ID; mismatch → 403, invalid/expired/wrong
  key/alg/iss/aud → 401. See [username-jwt-swagger.md](username-jwt-swagger.md).
* Everything else needs exactly one `Authorization: Bearer <token>`.
  Missing/unknown/oversized → **401** (`WWW-Authenticate: Bearer`).
* A **user** credential binds one user ID on the server. Sending
  `X-Acting-User-ID` with it is **403**.
* A **service** credential must send exactly one canonical positive decimal
  `X-Acting-User-ID` (no sign, leading zero or whitespace); anything else is
  **403**. The acting user must be in `allowed_user_ids`, otherwise **403**.
  Ownership comes only from the verified principal — never from JSON or query.
* Query strings are rejected (**422**). Unknown paths/methods are **404**.
* Ownership is checked before any side effect or backend call. A foreign,
  unknown or malformed ID is an identical **404** `not_found` (VMs, images,
  operations, SSH, terminal, delete). Bot allowlisting and API ownership are
  **not guest-network isolation**; guests of different users share whatever
  network the backend gives them.

## Routes

All bodies are strict JSON (unknown fields and trailing data rejected, ≤16 KiB).
Mutations require exactly one `Idempotency-Key` (1–128 printable ASCII
characters, no spaces), scoped to the principal's user ID.

| Method & path | Body | Success |
| --- | --- | --- |
| `GET /healthz` | – | 200 `{"status":"ok"\|"degraded"}` |
| `GET /v1/capabilities` | – | 200 `{"max_vms","max_vms_per_user","backend_max_vms","profiles":[{"name","vcpu","memory_mib","disk_mib"}],"max_total_memory_mib"?,"max_total_vcpu"?,"ssh_public_key_supported":false}` |
| `GET /v1/vms` | – | 200 `{"vms":[VM]}` (caller's own only) |
| `GET /v1/vms/{id}` | – | 200 VM |
| `POST /v1/vms` | `{"name"?,"image_id","resources"?:{"vcpu","memory_mib"},"ssh_public_key"?}` | 202 `{"operation_id","vm_id"}` |
| `POST /v1/vms/{id}/stop` | `{}` | 202 `{"operation_id","vm_id"}` |
| `POST /v1/vms/{id}/commit` | `{"name"}` | 202 `{"operation_id","vm_id","image_id"}` |
| `POST /v1/vms/{id}/terminal` | `{}` | 202 `{"operation_id","vm_id"}` |
| `DELETE /v1/vms/{id}` | – | 202 `{"operation_id","vm_id"}` |
| `GET /v1/vms/{id}/ssh` | – | 200 `{"host","port","user","bastion"?,"host_key_fingerprint","ready","auth_mode","key_reference"}` |
| `GET /v1/images`, `GET /v1/images/{id}` | – | 200 `{"images":[{"id","name","shared"}]}` / one image (only `ready` images) |
| `DELETE /v1/images/{id}` | – | 202 `{"operation_id","image_id"}` |
| `GET /v1/operations/{id}` | – | 200 `{"id","status","kind","vm_id"?,"image_id"?,"result"?,"error"?}` |

VM: `{"id","name","image_id","state","phase","resources":{"vcpu","memory_mib"}}`.
`phase` is the API's durable lifecycle (`creating`, `active`, `stopping`,
`stopped`, `committing`, `committed`, `deleting`); `state` is the last backend
state the API observed. Operation `status` is `pending|running|succeeded|failed`;
a `failed` operation carries `error:{code,message}` with a fixed safe message
(`operation not confirmed`) and one of `unavailable|conflict|capacity|invalid_argument`.
Terminal result when succeeded: `{"result":{"url":"https://…/t/<capability>/"}}`.

### SSH readiness

`GET /v1/vms/{id}/ssh` keeps connection metadata accessible while the guest
boots or is unreachable: a readiness failure returns **200** with `ready:false`,
not an error or a wait-until-ready operation. Failure to retrieve the metadata
itself still uses the normal error envelope. Foreign/absent VM IDs return 404
before any lifecycle access or network probe.

`ready:true` means a **fresh TCP connection from the API host** to the returned
`host:port` succeeded while the API phase was `active` and the live backend VM
was `running`. Live state is checked before and after that connection, and the
durable phase is rechecked to fence stop/delete intent. Non-active phases
(including `stopping`, `stopped`, `committing`, `committed`, `deleting`), missing
host/port, missing/unavailable/stopped backend state, failed connection and probe
timeout all yield `false`. The API ignores any stale readiness bit in metadata.
This is a point-in-time observation, not a guarantee for a later connection.

There is one connection attempt, no boot/readiness polling. The complete
readiness observation, including backend state checks, gets at most **one
second** after metadata retrieval and cannot exceed the remaining request/query
deadline. The whole read honors caller cancellation and the controller's query
timeout (default **20 seconds**). No lifecycle lock is held while probing.

This does **not** authenticate SSH, verify a host key, test the bastion, or prove
that a laptop can route to the guest. Consumers requiring authenticated or
end-to-end readiness must establish their own SSH connection (using the returned
bastion metadata when needed). No guest credentials are issued by this GET.

### Status codes and error envelope

`{"error":{"code":"…","message":"…"}}`; messages are fixed safe text that never
include tokens, paths, foreign objects or backend detail.

| HTTP | `code` | When |
| --- | --- | --- |
| 401 | `unauthorized` | absent/invalid bearer |
| 403 | `forbidden` | acting-header misuse, user not allowlisted |
| 404 | `not_found` | unknown, foreign or malformed resource; creating from an image the caller cannot see (including another owner's derived image) |
| 409 | `conflict` | idempotency key reused with a different request; duplicate VM name or image name for the owner; action not valid in the current phase or another operation is pending/running on the VM; deleting a shared/in-use image |
| 422 | `invalid_argument` | strict-JSON/validation failure, missing or malformed `Idempotency-Key`, query string, `ssh_public_key` (unsupported, see below), zero resources |
| 429 | `capacity` | the API's global or per-user quota at admission (backend exhaustion appears later as an operation error `capacity`, not as HTTP 429) |
| 503 | `unavailable` | any other failure |

### Idempotency and durability

Accepting a mutation atomically persists identities (VM/image/operation IDs),
quota reservation, owner and destructive intent in `state.json` **before**
enqueueing the DBOS workflow and before returning 202. The same key with the
same normalized request returns the same operation/resource, also after a
crash; a different request under that key is 409. IDs are generated once at
admission, never inside a retried workflow step. DBOS workflow IDs are stable
(`control-<operation>`). The caller disconnecting never cancels accepted work
(effects use `context.WithoutCancel` with an internal bound). The API has no
in-memory task map or custom scheduler. A result may arrive long after the HTTP
caller stopped waiting; poll `GET /v1/operations/{id}` (ownership of an
operation is checked independently of whether its VM still exists).

### Constraints

* **Resources.** `resources` = exact `vcpu` and `memory_mib`, both positive. If
  omitted, the configured `default_profile` is used. The API never sends a
  profile together with explicit resources, never clamps and never picks the
  nearest profile. The backend enforces policy: explicit maxima are the
  component-wise maxima of its configured profiles, bounded by host authority
  (`control-api-backend.md`). An out-of-policy request is accepted for durable
  admission (the API cannot see backend limits) and the operation then fails
  with `invalid_argument` or `capacity`; the VM record stays occupied until the
  caller issues an explicit Delete. Disk size is separate and is not resized.
* **`/v1/capabilities` is a live forward of backend policy.** Each request calls
  `vmlib.Service.Capabilities` (backend `Capabilities` → `vmlib.Service` → API,
  in-process); nothing is cached or hardcoded. `profiles` lists the
  backend's advertised profiles (`vcpu`, `memory_mib`, `disk_mib`), which also
  define the per-VM maxima for explicit `resources`. `max_total_memory_mib` and
  `max_total_vcpu` are the backend aggregate caps and are **omitted** when the
  backend does not enforce one (0). `max_vms` and `max_vms_per_user` remain the
  API's own global/owner quotas and are never replaced; the backend's own VM
  count is reported separately as `backend_max_vms`. SSH/bastion metadata is not
  part of this response. The values are advertised policy, not a promise that the
  host can currently allocate them; if the backend is unreachable the call
  returns `503 unavailable`.
* **`ssh_public_key` is rejected (422) today.** The current local backend only
  supports SSH keys preinstalled in the image; an optional caller key is not
  honored, so the API refuses rather than silently ignoring it. No private key
  is ever distributed through the API.
* **Image keys.** User-supplied image URLs/paths are not accepted; `image_id`
  must be a configured shared image or a `ready` private image of the caller.
* **Derived images.** Named per owner (unique within the owner, repeatable across
  owners); IDs are globally unique, generated once at commit acceptance.
  Another owner cannot see, boot or delete them. `DELETE /v1/images/{id}` is
  refused (409) for shared bases and for any image referenced by a VM record in
  any phase; the backend independently refuses referenced/in-flight images.
  There is no force removal.
* **Commit is a cold commit.** The VM is stopped and its disk published as a new
  immutable image; no RAM snapshot. The source VM becomes `committed`, stays
  stopped and keeps occupying its quota/capacity slot until an explicit Delete.
  Create from the new image after the commit succeeds (and a slot is free).
* **Stop/Commit/Delete revoke terminals first** (before the intent is
  enqueued), and a Terminal admission rechecks phase, identity and that the
  owner is still allowed. The terminal URL is a **bearer capability**: anyone
  holding a copy has a shell until revoked. It is not identity-bound.
* One non-delete operation per VM at a time (409 otherwise). Delete may
  supersede queued work.

## Startup repair, degraded mode and failure retention

On start the process opens the store (exclusive `flock` lifetime lock; second
process fails), launches DBOS and runs `RecoverControl`, which re-drives
pending/running operations with stable IDs, repairs the JSON↔SQLite enqueue gap
and re-establishes terminal projections only for active VMs whose owner is still
allowed. If recovery returns an error (for example an operation could not be
re-enqueued), vmapi **keeps serving**: durable status stays queryable and
`/healthz` reports `degraded`, with a one-line log that omits error text. A
restart re-runs repair. There is no automatic periodic retry in v1. Shutdown
closes projections and DBOS before releasing the lock.

Uncertain outcomes keep occupancy: failed create/delete/stop operations leave the
VM record and its quota slot in place; the caller retries by issuing a new
explicit request (new key) or Delete. Nothing is auto-retried with a new random ID.

### Image deletion and retry

`DELETE` of a private image durably moves it to phase `deleting` before the
backend call. A `deleting` image stays visible to its owner in `/v1/images` with
`phase: "deleting"` (visibility is not boot eligibility): it can never be used
to create a VM, and another owner never sees it. If the backend call fails, the
operation is `failed` and the image stays `deleting`. Replaying the **same key**
returns that original failed operation (no new attempt). A **new key** starts a
fresh attempt, but only once every earlier delete operation for that image is
terminal (otherwise `409`); the backend `DeleteImage` is idempotent, so a retry
after a lost response against an already-absent image succeeds. Shared images,
unknown images, images still referenced by a VM and `publishing` images (see
below) are never deletable through this path. This does not change commit
publication ambiguity.

### Commit publication ambiguity (manual recovery)

`Lifecycle.Commit(vm, image_id)` does not expose source/publication provenance
across a backend restart, so after a crash between the publish effect and the
operation checkpoint (or an `AlreadyExists` collision) the API **cannot** tell
its own half-finished image from a pre-existing one. It therefore never adopts
an image: the operation ends `failed`, the derived image record stays
`publishing` (private, invisible in `/v1/images`, not bootable, not deletable
through the API, name reserved), the source VM stays `committing`, and blind
re-commit under that name is refused. This is a documented **manual recovery**:
an operator inspects the backend catalog, decides whether the published image is
the intended one, and cleans up the backend/state out of band (or Deletes the
source VM). Transparent exactly-once commit recovery needs backend-held
provenance (see gaps).

## Migration from the legacy bot and handoff

The legacy bot (baseline `a1ce0dd`) and vmapi must never run against the same
state directory at once; the lifetime lock enforces that for vmapi/legacy.

1. Plan with the operator; keep the old binary, config, unit and an offline
   copy of `state_dir`. Nothing below happens automatically.
2. Stop **and disable** the legacy Telegram poller; verify it is gone.
3. Run the frontend's one-time offset import from the stopped legacy state into
   the new frontend delivery directory (`bot-api-client.md`). Do this **before**
   starting vmapi on that directory, because both take the same `lock` file and
   the import preserves the high-water update offset so the token switch cannot
   replay old Create commands.
4. Set `legacy_owner_user_id` and start vmapi on that `state_dir`. For a legacy
   v1 or v2 file with VMs and no configured owner, startup fails before writing
   anything. Otherwise the exact legacy bytes are saved to a private never-
   overwritten `state.json.v1.bak[.N]`, all existing VMs get that owner, and ID,
   token, generation, phase, `Seq` and offset are preserved (existing capability
   links keep working for the owner). Restarting with another owner never
   reassigns. Over-quota legacy resources are retained and count.
   A legacy VM already in `deleting` (deletion intent persisted before its
   workflow was enqueued, or a queued legacy workflow) gets one stable
   owner-scoped control `delete` operation (key `legacy-delete:<vm>:<seq>`)
   written in the same atomic state write as the migration; `RecoverControl`
   repairs the same gap for stores migrated earlier and never adds a second one.
   Legacy workflow bodies are retired without any effect, never resumed. A
   legacy `creating` VM is only reconciled through `Get` (never a fresh
   `Create`); if the backend lacks it, the refresh fails and it stays `creating`
   until an explicit Delete.
5. Verify `GET /v1/vms` with the owner's token, then start only the new
   frontend. Rollback is not "restart the old bot": the baseline state decoder
   rejects unknown fields (code reading; not executed here), so it refuses
   control state, and its stale offset would replay commands anyway.

## Verification scope

Passing tests (root/KVM-free) include: real HTTP server + real DBOS SQLite with
fake backend effects (idempotency replay/conflict, foreign IDs, quota, terminal
fences, crash/kill recovery, caller disconnect), v1/v2 migration, public client
against the actual handler, and an **executable smoke** that runs the real
`vmapi` `run()` in-process with the in-memory `vmlibtest` fake injected through a
test-only opener seam (the deployed binary only contains the real local backend): health/auth, create with resources, poll, SSH,
cold commit, delete, create from the derived image, image protection, delete
image, restart. **Not covered:** Firecracker/KVM, real guest boot or SSH login,
real ttyd/ssh terminals (unit-tested with fakes only), a live local backend (host helper/systemd),
production data, live Telegram.

```sh
make control-build && make control-smoke
GOMAXPROCS=2 go test ./internal/controlapi ./internal/vmbot ./cmd/vmapi ./controlclient
```

## Known gaps / uncertainty

* Commit provenance (above): no transparent recovery; manual procedure only.
* Caller SSH public keys unsupported by the current backend.
* Startup recovery is one-shot per process; degraded state clears only on restart.
* Failed operations hold their quota slot until an explicit Delete.
* `vmapi` operation timeout is the controller default (5 minutes) and is not yet
  configurable.
* No audit log, retention/pruning of finished operations or DBOS rows, or
  rate limiting beyond bounded request sizes/timeouts.
* A terminal URL can be forwarded; revocation is the only control.
