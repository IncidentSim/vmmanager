# Control API backend additions

These are source changes only. They do not alter deployed policy, services, networking, VM state or runtime files. The backend is a trusted-operator layer that `vmapi` now embeds in-process (`vmlib.Service` over `vmlib/local.Backend`); there is no VM RPC transport. Tenant ownership belongs to the control API, not this layer.

## Exact CPU/RAM selection

`vmlib.CreateRequest` and `PrepareRequest` accept `Resources *VMResources`, where `VMResources` contains `VCPU uint32` and `MemMiB uint32`.

- Existing nonempty named-profile requests remain valid. No implicit default profile is introduced: the application chooses its configured default when both inputs are omitted.
- An explicit resource request requires both values positive and `Profile == ""`. Supplying both modes is `ErrInvalidArgument`, even if they happen to describe the same size.
- The local per-VM explicit maximum is component-wise maximum CPU and memory of the **configured, validated profiles**. The existing advertised `Capabilities.Profiles` therefore defines these maxima. There is no nearest-profile substitution, clamping or expansion of installed host authority.
- Every configured profile is already checked against `UnitMemLimitMiB` including `VMMOverheadMiB`, and against aggregate limits. The unit-memory addition is widened to avoid overflow. The CPU limit is admission count, not execution quota; existing cgroups still govern execution.
- Explicit CPU/RAM does not select or resize a disk. Its image-size allowance is the **smallest configured profile's DiskMiB**. Named profiles retain their original individual disk bounds. Copying an image does not grow its filesystem.
- `VM.Resources` and `HostVM.Resources` are values containing the actual effective reservation. They are part of the in-process `vmlib` types, including introspection/reconciliation. `Profile` stays empty for explicit-resource VMs; no synthetic profile is invented.
- Existing durable `meta.json` `vcpu`/`mem_mib` fields persist the exact reservation before copying. `Start` continues using those values rather than resolving the current profile. Restart preserves them even when a profile shrinks or disappears. An identical explicit Prepare retry returns the old reservation even after policy shrinks; changed resources for an existing VM ID are a conflict.
- Aggregate RAM/vCPU admission sums durable reservations in `uint64` under the backend table lock. Preparing, stopped, committed, uncertain and retained workspaces all count until safe `Release`. Legacy metadata without a reservation still receives conservative component-wise profile maxima.

## Managed image deletion

`DeleteImage(ctx, id) error` is implemented by `Lifecycle`, `Backend`, `Service`, the local backend and `vmlibtest.Backend`.

- Validate opaque IDs before filesystem access. Ordinary missing image IDs are idempotent success.
- Only regular catalog images with regular `.committed/<id>` publication markers are deletable. Unmarked images are immutable bases. The backend does not infer tenant ownership from a marker; API ownership and explicitly configured shared-image protection remain required.
- A reference from **any** workspace blocks deletion, including preparing, stopped and uncertain VMs. Unreadable workspace metadata blocks all image deletions because its references cannot be proven absent. Restart reconstructs these barriers from durable workspace metadata.
- The backend table mutex serializes source-image lookup and Prepare admission with deletion. Once an entry is admitted, its immutable source reference protects the image throughout copying and the VM's lease. If deletion wins first, Prepare sees NotFound and cannot admit a stale source.
- Existing target-publication reservations use the same mutex. Delete refuses a target with an active publisher, even before the image exists. A Prepare of a newly visible image whose publisher still holds its reservation is definitively refused with `ErrResourceExhausted`, preserving Service's existing settled-creation semantics rather than fabricating an uncertain VM.
- Image deletion conflicts (base, referenced, publication, unreadable references) return existing `ErrNotStopped`; invalid paths/symlinks return `ErrInvalidArgument`. The API maps them to HTTP 409/422 envelopes. No new error sentinel or authorization behavior is introduced.
- The image root and managed-marker directory are opened without following symlinks. `fstatat(AT_SYMLINK_NOFOLLOW)` validates entries, and descriptor-relative `unlinkat` removes only the catalog entries, never a symlink target. Backend-owned roots must not be mutated by another operator concurrently; the existing ownership flocks remain held through the operation and Close drains it.
- The image unlink is synced before its classification marker is removed and synced. A marker without an image can be an interrupted publication or deletion: deletion fails closed with `ErrNotStopped` and preserves that evidence for operator reconciliation. Thus a normal completed deletion is idempotent; arbitrary power loss between these two durable steps can require operator review. There is no force-removal path or automatic cleanup of uncertain publication artifacts.

## Private configuration mapping (former `vmd` -> `vmapi`)

The gRPC plane, `cmd/vmd`, the protobuf definitions and the generator were removed. The backend policy keeps its exact shape and validation, now defined once in `internal/localcfg` (`JSONConfig`, strict decode, `Validate`, and a linux-only `Assemble`/`Open` that builds the real helper launcher and Firecracker SDK VMM). The object that `vmd` read at `.backend.local` is now the top-level `"backend"` object of the `vmapi` JSON config, field for field:

| old `vmd` config | new `vmapi` config |
|---|---|
| `backend.kind: "local"` | removed (only a local backend exists; no fake in the deployed binary) |
| `backend.local.<field>` | `backend.<field>` (same names: `work_root`, `images_dir`, `run_dir`, `max_vms`, `helper`, `ssh_user`, `min_free_mib`, `storage_mount`, `max_total_mem_mib`, `max_total_vcpu`, `unit_mem_limit_mib`, `vmm_overhead_mib`, `guest_dns`, `guest_prefix`, `pool_conf`, `profiles[]`, `bastion`) |
| `backend.kind: "remote"`, `backend.remote.*` | removed |
| `serve_backend`, `serve_facade` (listen/ca/cert/key) | removed; no VM listener exists (old public 19450 / private 19451 stop being served) |
| vmapi `facade` {addr, server_name, ca_file, cert_file, key_file} | removed; a config that still contains `facade` is **rejected** with an explicit error, never silently ignored |

One-liner migration (review the result, keep the old files for rollback):

```sh
(umask 077; python3 -c 'import json,sys; v=json.load(open(sys.argv[1])); a=json.load(open(sys.argv[2])); a.pop("facade",None); a["backend"]=v["backend"]["local"]; json.dump(a,open(sys.argv[3],"x"),indent=2)' \
  old-vmd.json old-vmapi.json new-vmapi.json)
```

Validation notes: unknown backend fields (including any attempt to set the test-only `trusted_owner_uid`/ancestor seams) are rejected; the seams exist only in Go test code. `helper` must be absolute; `storage_mount`, `guest_prefix`, `pool_conf` and the aggregate admission caps are required exactly as before; a nonempty caller `ssh_public_key` is still refused (no caller key injection) and API SSH metadata never contains private keys. `max_vms` in the API policy must not exceed `backend.max_vms`.

### Lifetime locks and handoff

`local.New` takes exclusive non-blocking flocks on `work_root`, `run_dir` and `images_dir`. The embedded backend therefore **cannot start while the old `vmd` (or another API) is alive**: stop the bot and API, then `vmd`, back up the whole state directory (JSON + SQLite + WAL together, service stopped) and only then start the new `vmapi`. `Close` releases the locks only; it never stops or deletes a VM. VM identities, slot leases, profiles, aggregate reservations, publication markers and receipts are all read from the existing durable metadata, so existing VMs and terminal capability links keep working. Do not restore an old state snapshot over state the new API has written; a rollback after new-format or local effects needs a current-state compatibility review.

### Honest isolation limit (proof of concept)

`vmapi` holds only the **public** JWT verification key, but it runs as the same OS user that owns the helper client, the SSH key file and the state files, so any process with that UID can read those private files. The user-unit template documents `ProtectHome`/`BindPaths`-style hardening only as a goal: on the reference host unprivileged user namespaces were observed to be unavailable, so such user-unit sandbox settings are **not enforced** and must not be relied on. There is no root/user isolation claim here; the guarantee is the helper's one-verb privilege boundary and root-owned runtime receipts, not same-UID file secrecy.

## Capabilities forwarding

`vmlib.Lifecycle` gains `Capabilities(ctx) (ResourceCapabilities, error)`; `Service` forwards the backend's `Capabilities` live (profiles copied, no cache) and the API reads it in-process. `ResourceCapabilities` carries only `MaxVMs`, `Profiles`, `MaxTotalMemMiB` and `MaxTotalVCPU` (0 = not enforced). Bastion, SSH user and auth mode are intentionally not forwarded here; they stay available per VM through `SSHInfo`. It adds no write path and no new authority.

## Verification boundary

Focused root/KVM-free Go tests exercise local durable files and catalogs, exact MachineSpec resources, restart accounting, overflow-safe policy, publication/preparation deletion barriers, and `vmlib.Service` directly over the real local backend (`vmlib/local` embedded-service test). Only the launcher/VMM seams are fake there; the vmapi smoke tests inject `vmlibtest` through a test-only opener seam as explicitly named. These tests prove source-level policy/transport behavior, not real Firecracker boot, installed cgroup enforcement, off-host access or deployment readiness. Existing early Delete tombstones, uncertain Prepare ownership, receipt checks and Close-drain barriers remain unchanged.
