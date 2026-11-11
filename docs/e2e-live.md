# Real live E2E (explicit opt-in)

`e2e/` uses the current `controlclient` against a deployed HTTPS API, real
Firecracker guests and real OpenSSH login. It is **not** a fake provisioning or
CPU-enforcement test. Ordinary tests skip live cases before reading fixture files
or issuing requests. No new live run is claimed by this change.

## Run (operator only)

```sh
export VMAPI_E2E=1
export VMAPI_E2E_BASE_URL=https://HOST[:PORT]
export VMAPI_E2E_USER_ID=123
export VMAPI_E2E_TOKEN_FILE=/private/owner-token
export VMAPI_E2E_BASE_IMAGE_ID=bootable-configured-image
export VMAPI_E2E_SSH_DIRECT=1
# Optional sole host trust source, if authenticated API fingerprint is unavailable:
export VMAPI_E2E_SSH_KNOWN_HOSTS_FILE=/private/trusted_known_hosts
GOMAXPROCS=2 go test -count=1 -p 1 -timeout 45m -v ./e2e
```

The host must route directly to the guest endpoint returned by the API. The base
image must satisfy the deployed offline SSH provisioning format (see
`docs/ssh-access.md`) and have working sshd before boot. No shared/default SSH
private key is used. `VMAPI_E2E_SSH_KEY_FILE` is a legacy optional setting, validated
if present but **not used for login**. Generated identities come from the owner-only
`GET /v1/vms/{id}/ssh-key`; supplied identities are freshly generated per test and
their real public keys are passed to Create. Private keys exist only in private
Go temporary directories, removed at test cleanup, and are never logged or added
to reports. Token files are never printed.

Create uses `profile` and `ttl_seconds`, never public `resources`. Normal fixtures
request min(600, advertised max TTL) seconds; tests fail if finite policy/default
profile is absent. Very short deployment maxima may not allow long lifecycle/SSH
fixtures to finish; this is a policy constraint, not a reason to disable TTL.

## Public SSH / automatic guest networking

`TestLiveGuestNetworkAndSlotReuse` creates two sequential disposable VMs through
`controlclient`; each is tracked before assertions and removed through normal
receipt-checked Delete. It checks the advertised public host/slot port/no bastion,
automatic default route and DNS (without modifying either), two actual HTTPS 200
responses, and timed-out guest connections to host SSH/API, metadata and a Docker
private address. It cooperatively reboots only its own guest before Delete.

In addition to the common variables above:

```sh
export VMAPI_E2E_NETWORK=1
export VMAPI_E2E_NETWORK_HOST=185.70.184.64
export VMAPI_E2E_NETWORK_PORT_BASE=22000
# Optional: require the second VM to reuse the first port; needs exclusive admission.
export VMAPI_E2E_NETWORK_REUSE=1
# HOST-LOCAL mode only: use explicit private /16 prefix plus advertised slot.
# Omit this when testing public SSH from an independent external machine.
export VMAPI_E2E_NETWORK_PRIVATE_PREFIX=10.213
GOMAXPROCS=2 go test -p 1 -count=1 -timeout 12m -v ./e2e -run '^TestLiveGuestNetworkAndSlotReuse$'
```

The host-local mode logs its limitation explicitly; it is NOT public-ingress
acceptance. Run without the private prefix from another machine for authenticated
public-endpoint SSH, using an operator-trusted known_hosts file for those public
host:port tuples. Sequential same-port reuse does NOT prove stale established
conntrack sessions cannot reach a successor. Cross-tenant live peers, IPv6/spoofed
packets and kernel conntrack-session retirement need separate acceptance cases;
source-level firewall/helper tests are not substitutes for them.

## Acceptance coverage

| Test | Real assertions |
|---|---|
| `TestPublicDocsHealthAndAuth` | Health, local Swagger assets/CSP, parsed OpenAPI equality with source after removing **only** dynamic `CreateVM.example`, unauthenticated/invalid bearer rejection, valid auth. No byte-equality claim for configured documents. |
| `TestConfiguredSwaggerCreateExample` | Fetch published configured `components.schemas.CreateVM.example`, reject unknown body fields, execute its exact image/profile/TTL values through the authenticated SDK without repairing/replacing the example; await boot and generated-key SSH login. Missing configured example fails. |
| `TestLifecycleStopCommitsAndConflicts` | Create/replay/conflict, stable generated key across repeated retrieval and Create replay, no-store header, actual login and marker, Stop, stopped Terminal conflict, two **sequential** cold Commits, images survive source deletion. |
| `TestSuppliedKeyLogin` | Supply a fresh real Ed25519 public key, actual login with its matching private key, generated-key retrieval returns `409 key_not_generated`. |
| `TestForeignKeyRetrieval` | Separately authenticated other user cannot retrieve owner keys through SDK or raw response. Requires explicit second-principal fixture below; otherwise explicit skip. |
| `TestDerivativeFromCommittedImage` | Marker persists; derivative independently logs in using its newly retrieved generated identity; in-use image deletion conflicts, then succeeds after derivative deletion. |
| `TestCommitCredentialHygiene` | Attach Terminal first, inspect original owner/terminal managed authorized entries over SSH; preserve a legitimate guest-added key, original non-managed lines and marker; cold Commit; derivative authenticates independently; neither original ephemeral entry survives, derivative credentials differ, old owner identity cannot log in. No rootfs/credential-store inspection. Requires documented managed entry tags; absence fails rather than claiming hygiene. Does not exercise browser/WebSocket terminal. |
| `TestFiniteTTL` | Capture before/after HTTP Create bracket; require deadline within `[before + requested TTL - 2s, after + requested TTL + 2s]` and matching effective TTL. Poll for ID-scoped 404 with a one-minute bound, rejecting any observed disappearance before `before + requested TTL - 2s`; optional restart verifies deadline not reset (already-expired readback explicitly leaves persistence NOT proven). Requests min(30,max) seconds. |
| `TestTTLBusyCreate` | One-second real Create, attempt to observe expired record while child operation remains pending/running; ordinary command must return `vm_expired`, then VM disappears. If no overlap can be observed, cleans fixtures then explicitly skips: busy-child deferral was **not proven**. No forced kill or backend delay injection. |
| `TestProfileAndQuotaMetadata` | Positive quotas, generated/supplied support and default CPU cap; VM effective profile/vCPU/memory/CPU match capabilities. **Metadata is not cgroup evidence.** |
| `TestOptionalPerUserQuotaAdmission` | Separate quota-saturation opt-in, at most four owned fixtures, one more Create returns 429 and count unchanged. Does not distinguish per-user/global/backend capacity when those bind simultaneously. |
| `TestOptionalPhysicalCPUReadback` | Separate host opt-in and trusted VM→PID resolver; independently read actual Firecracker PID's cgroup v2 `cpu.max`, require isolated single-process cgroup and finite ratio equal to effective cap. No API JSON substituted for kernel readback. |
| `TestEarlyDeleteAfterCreate` | Queued early deletion waits for Create then removes the owned VM. |
| `TestOptionalAPIRestart` | Explicit user-unit restart; same VM/deadline, endpoint, key, guest boot ID and marker. Does not prove VMM process identity. |

The TTL check permits an explicit two-second client/server clock skew and timestamp
rounding tolerance, not an arbitrary shortened lifetime: a one-second deadline for
a thirty-second request fails. Polling only constrains **observed** disappearance;
it does not prove the exact deletion instant between reads or during restart.
For deployment maxima shorter than the tolerance, the lower-bound timing evidence
is necessarily weak; the requested/effective TTL and deadline bracket still apply.

## Separate destructive/host gates

Second user (not inferred from host/private configuration):

```sh
export VMAPI_E2E_OTHER_USER_ID=456
export VMAPI_E2E_OTHER_TOKEN_FILE=/private/distinct-user-token
```

Both must identify a valid **distinct** ordinary principal. Missing both gives an
explicit isolation skip; partial/invalid fixtures fail. No second-user mutations
or token minting occur.

Restart is destructive to concurrent API requests and must be separately approved:

```sh
export VMAPI_E2E_RESTART_USER_UNIT=firecracker-vmapi.service
# additionally include a restart DURING the short-TTL fixture:
export VMAPI_E2E_TTL_RESTART=1
```

Only the explicit plain unit name is passed to `systemctl --user restart`, bounded
to 90 seconds; no sudo/system service control. `TTL_RESTART=1` without the unit
fails when the TTL test is enabled. Deadline equality is checked if the record
still exists; an already expired 404 explicitly skips because it is not restart-persistence
proof. Run with enough TTL/startup margin to observe the persisted record.

Quota saturation is separately enabled with `VMAPI_E2E_QUOTA=1`. Existing owned
VMs consume headroom but are **never deleted**. Other users' invisible occupancy
can prevent the fixture from filling; report that failure/skip honestly.

Physical CPU readback (on the actual deployment host only):

```sh
export VMAPI_E2E_HOST_READBACK=1
export VMAPI_E2E_VMM_PID_RESOLVER=/absolute/operator-owned/read-only-vm-to-vmm-pid
```

The resolver is an explicit trusted executable, not writable by group/world,
called directly with **one VM ID**. It must output **only** that VM's positive
Firecracker PID, or fail. It must be read-only; never launch/restart/kill a VM or
change a quota. The harness does not supply a guessed slot mapping, inspect
private backend state or run sudo. Operator must independently validate this
mapping for the deployed launcher; it is a stated trust boundary. The harness
checks `/proc/PID/exe`, `/proc/PID/cgroup`, `/sys/fs/cgroup/.../cgroup.procs` and
`cpu.max` itself. Unsupported cgroup v1, unreadable proc/cgroup, shared cgroup,
unlimited CPU or wrong ratio **fail once opted in**, not pass from metadata.
Without the gate it explicitly skips. This proves configured kernel quota for the
operator-resolved VMM, not measured CPU throughput under stress. Root access, if
needed to make those files readable, requires separate operator approval outside
this harness; no automatic elevation exists.

## Host-key policy (never relaxed)

`StrictHostKeyChecking=yes`, `IdentitiesOnly=yes`, `BatchMode=yes`, password auth
off, agent forwarding off, no connection multiplex reuse, global known_hosts
ignored. Without the optional trusted file, `ssh-keyscan` keys are admitted only
when their SHA256 fingerprint equals the TLS-authenticated API fingerprint. Empty
or mismatching fingerprints fail. With `VMAPI_E2E_SSH_KNOWN_HOSTS_FILE`, that
operator-trusted regular file is the **only** trust source, used verbatim. There
is no TOFU, `accept-new`, or `StrictHostKeyChecking=no`; non-22 ports need trusted
`[host]:port` entries. TLS uses default certificate verification; redirects are
not followed. No secrets are sent to another origin.

## Cleanup and limits

Tests are serial; each has an eight-minute context and fresh five-minute cleanup
context. Preflight snapshots this user's VM/image IDs, reads caps/base image and
skips visibly on known capacity exhaustion. It does not assume a globally empty
host. Every returned nonempty Create/Commit VM/image ID is tracked before waits
or success assertions, including replay/conflict unexpected successes. All VM
and image deletes are ID-scoped and guarded against baseline/preexisting/base
images and untracked IDs; cleanup never performs bulk deletion. Sequential cold
Commits remain mandatory. Errors are reported as test failures; leftover IDs are
not silently dropped. A response lost before an ID is returned cannot safely be
cleaned by enumeration; no broad deletion is attempted.

## Verification status / remaining live evidence

The new suite is compiled and its harness unit tests run with `VMAPI_E2E=0`.
**No live endpoint, private service configuration/credentials, root actions or
restart was used during development.** Historical five-test evidence at revision
`913ad5d` predates these profile/TTL/personal-SSH checks and does not validate them.

Remaining acceptance: operator deploys the integrated revision with supported
image, encrypted credential store, profiles and enforced CPU cgroups; supplies
trusted host keys/direct routing and ordinary token fixtures; runs all real SSH,
Swagger, Commit, TTL and cleanup cases; explicitly enables second-user isolation,
restart/TTL restart and quota gates; provides audited VM→PID readback transport.
A busy-child overlap must actually be observed (otherwise reported unproven),
and short-TTL restart must retain a readable unchanged deadline to count as
persistence evidence. Browser ttyd/WebSocket use, off-host SSH, power loss,
process-level no-preemption and global quota isolation remain outside these tests.
