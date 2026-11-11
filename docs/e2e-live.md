# Live E2E tests (opt-in)

`e2e/` contains Go tests that drive a **real, deployed** `vmapi` over its public
HTTPS API (reusing `controlclient`) and log into real guests with the system
`ssh` client. They create and delete real VMs and images. They are separate from
the fake-backend tests (`make check`, `make control-smoke`), which stay unchanged.

Without `VMAPI_E2E=1` every live test skips; `go test ./...` only runs the pure
harness unit tests (config validation, naming, polling, cleanup against a fake).

## Run

```sh
export VMAPI_E2E=1
export VMAPI_E2E_BASE_URL=https://HOST[:PORT]       # https only, no path
export VMAPI_E2E_USER_ID=<positive integer>          # user the token belongs to
export VMAPI_E2E_TOKEN_FILE=/path/to/token           # file with one bearer token
export VMAPI_E2E_BASE_IMAGE_ID=<image id>            # bootable, ready, preinstalled with the SSH key below
export VMAPI_E2E_SSH_KEY_FILE=/path/to/id_key        # private key (mode 0600) accepted by that image
export VMAPI_E2E_SSH_DIRECT=1                        # acknowledge the routing boundary below
# optional:
export VMAPI_E2E_SSH_KNOWN_HOSTS_FILE=/path/to/trusted_known_hosts   # see host-key policy
export VMAPI_E2E_RESTART_USER_UNIT=firecracker-vmapi.service
make e2e-live        # = go test -count=1 -p 1 -timeout 45m -v ./e2e
```

`make e2e-live` refuses to run unless `VMAPI_E2E=1`. It is not part of
`make check`. With `VMAPI_E2E=1` a missing/invalid variable is a hard failure
(never a silent skip). Secrets are only read from files; the token and key are
never put in Git, logs, or error messages. Output logs resource IDs only.

## What is tested (each test serial, bounded to 8 min; cleanup bounded to 5 min)

| Test | Checks |
|---|---|
| `TestPublicDocsHealthAndAuth` | `/healthz`; `/docs` page references only local assets, CSP present; Swagger CSS/JS/initializer/favicon served; unknown asset path not served; `/openapi.yaml` parses and is byte-identical to this checkout's `docs/control-api-openapi.yaml` (and parses equal); unauthenticated GET/POST and an invalid bearer get 401. Read-only; creates nothing. |
| `TestLifecycleStopCommitsAndConflicts` | Create 202; same key+body replays same VM/op; same key, changed body → 409; bounded SSH readiness; real guest login + marker file; Stop → `stopped`, readiness false; Terminal on stopped → 409; two cold Commits from the stopped VM, submitted serially (first awaited before the second), both become ready; deleting the source keeps both images; base image untouched. |
| `TestDerivativeFromCommittedImage` | marker written, stop, commit, delete source, new VM from the image reads the exact marker; deleting the in-use image → 409; after deleting the derivative the image deletes and reads 404. |
| `TestEarlyDeleteAfterCreate` | Delete admitted right after Create; delete op succeeds, VM ends 404. |
| `TestOptionalAPIRestart` | only with `VMAPI_E2E_RESTART_USER_UNIT`, see below. |

Names are `e2e<6 hex>-<tag>` (≤20 chars, API label pattern); idempotency keys are
unique per call. At most one VM per user is live at a time per test.

## Safety rules implemented

* **Capacity before side effects:** `GET /v1/capabilities`, the user's VM list and the
  base image are checked first. If the user has no headroom (per-user / API cap) or no
  profile fits the aggregate caps, the test **skips with a `SKIP (capacity)` message**.
  A missing/not-ready base image fails. No global empty state is assumed; other users'
  VMs elsewhere are not visible to the test, so a backend that is full for other
  reasons surfaces as a 429/capacity failure from Create (the test fails, then cleans up).
* **Cleanup:** every ID returned by an admitted Create/Commit is registered immediately,
  *before* waiting, and kept even if the wait fails. Cleanup runs on a fresh bounded
  context, deletes only those IDs (VMs, then images), refuses the base image, tolerates
  404, retries image delete on 409 while a VM reference drains, and reports every error
  with `t.Errorf` (a failed cleanup fails the test; leftovers are listed by ID).
  Preexisting resources and the base image are never deleted.
  A baseline of the user's VMs and images is captured in preflight; an admitted ID that is
  empty, the base image, or preexisting is refused (never tracked, never deleted), and every
  test-side image deletion goes through the same base/preexisting/untracked guard. A mutation
  that unexpectedly succeeds (idempotent replay, changed-body create) has its new VM ID
  tracked before the assertion fails. A lost/unknown mutation response is a documented limit:
  untracked resources are never deleted.
* No `state.json` reads, no filesystem/DB access to the service, no service control
  by default.
* TLS verification is the Go default (no bypass; redirects are not followed). The
  token goes only to `BASE_URL` via `controlclient`.

## Guest SSH: routing boundary and host-key policy

The test SSHes to the **host:port returned by the API**, directly. That address is a
private guest address; it is only reachable from the deployment host (or a network
that routes to it). Therefore run on that host and set `VMAPI_E2E_SSH_DIRECT=1`
explicitly. Bastion metadata is not used; running from a laptop is unsupported.

Host keys: `ssh-keyscan` fetches the guest's keys, and only a key whose SHA256
fingerprint equals `host_key_fingerprint` from the (TLS-authenticated) API is written
into a private temporary `known_hosts`; `ssh` then runs with
`StrictHostKeyChecking=yes`, `IdentitiesOnly`, `BatchMode`, password auth off and the
global/user known-hosts ignored. If the API gives no `SHA256:` fingerprint or none
matches, the test fails.

Optional `VMAPI_E2E_SSH_KNOWN_HOSTS_FILE`: an operator-trusted `known_hosts` file
(must be a readable regular file; contents are never logged). When set it is the
**only** host-key trust source: `ssh` uses it directly with
`StrictHostKeyChecking=yes`, no `ssh-keyscan`, and the API fingerprint is not
consulted (the real local backend may return it empty). Trust boundary: the
operator vouches that the file holds the right keys for the guest `host:port`
(use `[host]:port` entries for non-22 ports); a guest with no matching entry fails
the SSH handshake. Nothing is auto-trusted or accepted on first use. When unset,
the API-fingerprint matching above applies unchanged. `StrictHostKeyChecking=no` is never used. `ssh`/`ssh-keyscan`/
`ssh-keygen` are executed directly (no shell); the remote command is a fixed string
with a random hex marker.

## Optional API restart test

Runs only when `VMAPI_E2E_RESTART_USER_UNIT=NAME.service` is set (validated as a plain
unit name). It executes `systemctl --user restart NAME.service` with a 90 s timeout
(needs a user session, i.e. `XDG_RUNTIME_DIR`), waits for `/healthz`, then requires
unchanged VM record, SSH host/port/fingerprint, the guest marker, and the guest's
`/proc/sys/kernel/random/boot_id`. **Boundary:** this shows API-visible and guest-visible
continuity (the guest was not rebooted or recreated). It does **not** prove the VMM
process or its invocation was untouched; that needs host-side process inspection, which
these tests deliberately do not do. No service is touched without that variable.

## Evidence boundary

Proves, for the deployed revision: public HTTP/Swagger/OpenAPI behaviour and equality
with this checkout, authn rejection, idempotency/conflict semantics, real guest boot
and authenticated on-host SSH, cold-commit data persistence, image protection, early
delete, and cleanup of the objects the run created. Does not prove: reachability from
other hosts, browser terminal/WebSocket use (Terminal is only checked for the 409 on a
stopped VM), power loss, forced VMM kill, multi-user isolation, or VMM identity across
restart. See also `docs/vm-owner-live-verification.md` for the manual acceptance record.

## Verified live run

The complete opt-in suite was executed against the deployed cleanup revision
`913ad5d`, using real Firecracker guests, the production HTTPS endpoint and a
short-lived operator JWT read from a private file. The explicit trusted
known-hosts file was used because this backend returns an empty host-key
fingerprint. SSH still used `StrictHostKeyChecking=yes`.

All five live tests passed, without capacity skips:

* `TestPublicDocsHealthAndAuth`
* `TestLifecycleStopCommitsAndConflicts`
* `TestDerivativeFromCommittedImage`
* `TestEarlyDeleteAfterCreate`
* `TestOptionalAPIRestart` (explicit user-unit restart enabled)

`make e2e-live` exited 0 in 73.355 s. Cleanup completed for every tracked resource;
a separate operator inspection found zero VM records and no pending/running
operations, and the API user service remained active. No preexisting image was
deleted. Parent also ran full uncached `GOMAXPROCS=2 GOFLAGS=-count=1 make check`
after the E2E additions: exit 0; live tests were gated off in that ordinary check.

An initial live run failed safely because it required an API fingerprint that
this backend does not provide; all fixtures were cleaned. The operator-trusted
known-hosts option resolves that harness mismatch without disabling SSH checks.
These passing results are not browser WebSocket or off-host SSH evidence.
