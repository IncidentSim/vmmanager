# VM profiles, personal SSH, TTL and usable API implementation plan

**Status:** User approved defaults; implementation authorized, live activation subject to verified artifacts and root approvals. Base cf23f04. No changes to current live service during development.

**Goal:** Create usable temporary VMs with profile-selected enforced resources, personal SSH access, finite lifetime, useful validation and executable Swagger examples.

**Architecture:** Retain the sequential DBOS VM owner as the sole lifecycle writer. Keep shared quotas atomic and physical safety in the backend. Provision access before guest boot without editing base images or live mounted disks. Persistent credentials encrypted outside DBOS history. Profiles resolve into immutable effective reservations.

**Tech stack:** Go; pinned DBOS Go + SQLite; Firecracker/local host helper/systemd; go-playground/validator/v10 for DTO/config validation; golang.org/x/crypto/ssh for real public-key parsing and OpenSSH key encoding; stdlib authenticated encryption. No new actor/controller framework.

## Approved public contract

Create: POST /v1/vms, Idempotency-Key required. Body {name?,image_id,profile?,ttl_seconds?,ssh_public_key?}. Profile defaults small, TTL defaults 3600 and max 86400 seconds. Public resources input removed with useful unknown/unsupported field validation; update SDK, CLI and bot atomically. No duplicate API/backend profile lists; use existing backend profile catalogue as source, expose memory_mib consistently publicly (existing private mem_mib configs remain readable).

Profiles carry integer guest vcpu, fractional cpu_limit in units of CPU time, memory_mib and existing disk policy. Initial deployment small = 1 guest vCPU, cpu_limit=0.5, guest memory=500 MiB. Internal CPU limit accounting should use integer millicores/microseconds rather than floats for persisted arithmetic; reject nonfinite/invalid/unrepresentable values. Set actual per-VMM cgroup CPUQuota; the guest vcpu count is not the execution-time cap. Existing total host budget and VMM overhead remain separately enforced. Persist effective resources/profile on VM; config edits do not change existing reservations.

Quota defaults max_vms=4, max_vms_per_user=2. Retained/stopped VM occupy quota until proven-safe delete. Current host has 2 physical slots: development must prepare a narrowly scoped, auditable 4-slot deployment change; parent applies with approval after capacity review.

SSH: no provided public key => stable generated Ed25519 pair; provided key => parse genuine single OpenSSH key and install it. Before first boot configure per-VM access, without modifying base image, writing a live filesystem or using shared SSH bootstrap. Retain separate per-VM server access for ttyd. GET /v1/vms/{id}/ssh-key returns generated private key owner-only, Cache-Control no-store; supplied-key case must clearly say no generated key (proposed 409 key_not_generated). Keep credentials encrypted at rest with server file key; never plaintext in domain JSON, DBOS inputs/outputs/events, logs, list/capabilities. Retrieval works until VM deletion. Stable request identity prevents regeneration on replay. Commit must not publish temporary user/service authorized keys; preserve legitimate image content/user changes. Arbitrary images must explicitly satisfy documented provisioning format; never pretend all rootfs formats supported.

Terminal endpoint only starts/confirms ttyd SSH and returns a link. It does not install sshd or rewrite keys. Baseline images must contain working sshd. Web terminal can be attached independently of user-provided key.

TTL: persisted expires_at set from Create admission, no resetting on restart. Existing VM without deadline keep no expiry. Default/max configurable, no disable/renew endpoint initially. After deadline reject ordinary commands with vm_expired, owner requests safe Delete after current child settles; no preemption/kill/free on timeout. Avoid independent lifecycle controller, large per-second checkpoint polling, or wallclock branches that alter recorded history on replay. Use dbos.Patch/version compatibility for changed call sequence and test old owner histories. Expose expires_at in VM, effective ttl/profile/resources in API.

Validation: validator/v10 for forms/static policy, x/crypto/ssh for key syntax, domain-specific rules report field errors. Error envelope retains error.code/message, adds fields [{field,code,message}]; validation_failed, profile_unknown, vm_expired and key_not_generated map explicitly; don't expose underlying path/token/key/foreign resource info. Existing foreign/missing semantics retained. Unknown fields/types named; 405+Allow for wrong method on real routes. Expired/validation failures do not bypass idempotency/digest or ownership.

Capabilities expose actual profiles/default, default/max TTL, quotas, SSH supplied/generated key support; no advertised unimplemented feature. Swagger examples use a configured bootable image/profile, omit optional SSH key by default; configure self-hosted Swagger to supply idempotency key and maintain identity on retry. Live E2E executes the published create example after auth. Never put fake public keys or invalid values in executable defaults. Dynamic examples should be built from configured policy, not drift-prone hardcodes.

## Parallel workstreams and file ownership

Each workstream has a separate git worktree and may edit its own copy; do not commit/push/activate. Return focused tests plus exact changed files. Avoid editing shared HTTP/owner glue when assigned a lower layer; document exported integration seam instead. Parent integrates overlapping boundary edits deliberately, never copies whole files blindly.

A SSH backend: new key/credential module, filesystem provisioning and Commit hygiene, backend/terminal seams. Own SSH-focused files/tests. Public HTTP glue remains D/parent. If a backend.go hook is necessary keep minimal isolated diff; profile code is B. No live rootfs/state/config/credentials reads or root actions. Inspect existing build/deployment source and test a disposable mkfs.ext4 fixture; choose a proven offline provisioning mechanism, document assumptions and evidence. Preserve safe workspace/launcher/exit boundaries. No implementation pretending Create accepted key without actual install.

B profiles/resources: vmlib resource/capability types, local backend resource admission, localcfg, helper protocol and per-instance CPUQuota enforcement, new profile validation/tests. Prepare deployment templates/verification for 4 slots without executing root. Do not edit HTTP/client/owner TTL files; provide resolver/effective reservation seam to integrator. Coordinate any SSH backend hook via small nonoverlapping edits. Test persistent reservations across profile edits/restart and invalid CPU quota cases.

C TTL owner: domain expires_at/admission deadline policy, owner durable expiry path and compatibility tests, safe delete order after child. Own TTL-specific files and minimal control/state/owner changes. Do not replace lifecycle design or rewrite HTTP/client/profile/SSH. Config/request fields can be minimally introduced for wiring; parent reconciles shared structs. Real SQLite short-deadline tests including restart and busy child, legacy no-TTL and expired-command admission. No fake claims about clock/replay behavior.

D API/validation/clients/docs: DTO validator + HTTP error details, profile-only public contract, client/CLI/bot changes, Swagger and E2E updates, field-aware config validation. Work against explicit seams above; no reimplementation of B's physical CPU enforcement, A's secrets or C's expiry. New integration references must be documented if lower-layer symbols not yet available. Avoid circular dependencies/handwritten crypto. Preserve common image ownership/auth/idempotency.

## Stages and gates

1. Commit this RFC before parallel work; record scope and contracts. Workers baseline focused tests, add failing behavior tests before implementation. Each uses bounded focused tests (GOMAXPROCS=2), not concurrent full make builds.
2. Integrate reviewed lower-layer diffs into feat/vm-policy-access; resolve DTO/store/config changes centrally. Run static secret scan, full make check serially and independent spec/correctness/security review. No worker commits or changes another worktree.
3. Extend actual Go E2E with supplied/generated SSH login, key ownership/retrieval+retry, Commit hygiene, finite TTL/restart/busy child, caps/profile/CPU enforcement and configured Swagger example. Keep all cleanup ID-scoped; no deletion of preexisting resources/base images. Verify both expected and failure outcomes.
4. Prepare clean release build, backups and deployment-key/credential migration, inspect physical budgets. Root helper/unit/network-slot changes through sudo ask-exec with user-visible scope. No unrelated service/port modifications. Do not break old running VM identity or add TTL retrospectively.
5. Activate exact chosen revision, run E2E against real Firecracker, inspect cgroup settings and host resources, cleanup fixtures and report source/deployed revisions. Push only verified atomic commits, preserve linear history and provide .patch.

## Acceptance and blockers

A green fake suite is not provisioning/CPUQuota evidence; a successful profile response is not actual cgroup enforcement. Crypto generation/storage does not prove authorized_keys installation. Timeout never proves effect ended. SDK-history compatibility must have executable tests. If an agreed mechanism cannot be safely implemented, report a concrete blocker instead of silently substituting shared credentials, force kill, in-memory TTL, or uncapped fractions.
