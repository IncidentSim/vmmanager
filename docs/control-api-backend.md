# Control API backend additions

These are source changes only. They do not alter deployed policy, services, networking, VM state or runtime files. The API remains a trusted-operator backend behind the existing mTLS transports; tenant ownership belongs to the control API, not this layer.

## Exact CPU/RAM selection

`vmlib.CreateRequest` and `PrepareRequest` accept `Resources *VMResources`, where `VMResources` contains `VCPU uint32` and `MemMiB uint32`.

- Existing nonempty named-profile requests remain valid. No implicit default profile is introduced: the application chooses its configured default when both inputs are omitted.
- An explicit resource request requires both values positive and `Profile == ""`. Supplying both modes is `ErrInvalidArgument`, even if they happen to describe the same size.
- The local per-VM explicit maximum is component-wise maximum CPU and memory of the **configured, validated profiles**. The existing advertised `Capabilities.Profiles` therefore defines these maxima. There is no nearest-profile substitution, clamping or expansion of installed host authority.
- Every configured profile is already checked against `UnitMemLimitMiB` including `VMMOverheadMiB`, and against aggregate limits. The unit-memory addition is widened to avoid overflow. The CPU limit is admission count, not execution quota; existing cgroups still govern execution.
- Explicit CPU/RAM does not select or resize a disk. Its image-size allowance is the **smallest configured profile's DiskMiB**. Named profiles retain their original individual disk bounds. Copying an image does not grow its filesystem.
- `VM.Resources` and `HostVM.Resources` are values containing the actual effective reservation. They are transported on backend and facade gRPC responses, including introspection/reconciliation. `Profile` stays empty for explicit-resource VMs; no synthetic profile is invented.
- Existing durable `meta.json` `vcpu`/`mem_mib` fields persist the exact reservation before copying. `Start` continues using those values rather than resolving the current profile. Restart preserves them even when a profile shrinks or disappears. An identical explicit Prepare retry returns the old reservation even after policy shrinks; changed resources for an existing VM ID are a conflict.
- Aggregate RAM/vCPU admission sums durable reservations in `uint64` under the backend table lock. Preparing, stopped, committed, uncertain and retained workspaces all count until safe `Release`. Legacy metadata without a reservation still receives conservative component-wise profile maxima.

## Managed image deletion

`DeleteImage(ctx, id) error` is implemented by `Lifecycle`, `Backend`, `Service`, the local backend, both real generated gRPC services/clients and `vmlibtest.Backend`.

- Validate opaque IDs before filesystem access. Ordinary missing image IDs are idempotent success.
- Only regular catalog images with regular `.committed/<id>` publication markers are deletable. Unmarked images are immutable bases. The backend does not infer tenant ownership from a marker; API ownership and explicitly configured shared-image protection remain required.
- A reference from **any** workspace blocks deletion, including preparing, stopped and uncertain VMs. Unreadable workspace metadata blocks all image deletions because its references cannot be proven absent. Restart reconstructs these barriers from durable workspace metadata.
- The backend table mutex serializes source-image lookup and Prepare admission with deletion. Once an entry is admitted, its immutable source reference protects the image throughout copying and the VM's lease. If deletion wins first, Prepare sees NotFound and cannot admit a stale source.
- Existing target-publication reservations use the same mutex. Delete refuses a target with an active publisher, even before the image exists. A Prepare of a newly visible image whose publisher still holds its reservation is definitively refused with `ErrResourceExhausted`, preserving Service's existing settled-creation semantics rather than fabricating an uncertain VM.
- Image deletion conflicts (base, referenced, publication, unreadable references) return existing `ErrNotStopped`; invalid paths/symlinks return `ErrInvalidArgument`. These round-trip through existing gRPC FailedPrecondition/error metadata and InvalidArgument mappings. No new error sentinel or authorization behavior is introduced.
- The image root and managed-marker directory are opened without following symlinks. `fstatat(AT_SYMLINK_NOFOLLOW)` validates entries, and descriptor-relative `unlinkat` removes only the catalog entries, never a symlink target. Backend-owned roots must not be mutated by another operator concurrently; the existing ownership flocks remain held through the operation and Close drains it.
- The image unlink is synced before its classification marker is removed and synced. A marker without an image can be an interrupted publication or deletion: deletion fails closed with `ErrNotStopped` and preserves that evidence for operator reconciliation. Thus a normal completed deletion is idempotent; arbitrary power loss between these two durable steps can require operator review. There is no force-removal path or automatic cleanup of uncertain publication artifacts.

## Wire compatibility and generation

Existing protobuf tags and methods retain their identities. Additive `VMResources` messages and request/response fields carry resource presence exactly (an absent request field differs from a present zero-valued one). Both services add `DeleteImage`. Generated Go code comes from the repository's real pinned generator command:

```sh
GOMAXPROCS=2 bash scripts/gen-proto.sh
GOMAXPROCS=2 bash scripts/gen-proto.sh --check
```

No module dependencies changed. Old profile clients remain compatible. An older daemon does not acquire dynamic-resource or deletion support just because a new client was built; deploy compatible peers deliberately.

## Capabilities forwarding

`vmlib.Lifecycle` gains `Capabilities(ctx) (ResourceCapabilities, error)`; `Service` forwards the backend's `Capabilities` live (profiles copied, no cache) and `FacadeClient` calls the new additive facade RPC `VMService.GetCapabilities`. `ResourceCapabilities` carries only `MaxVMs`, `Profiles`, `MaxTotalMemMiB` and `MaxTotalVCPU` (0 = not enforced). Bastion, SSH user and auth mode are intentionally not forwarded here; they stay available per VM through `SSHInfo`. The RPC adds no write path and no new authority: it only exposes policy the backend already advertises to its privileged client. An older facade server returns gRPC `Unimplemented`, which the client maps through the existing error mapping; it is not synthesised into an empty catalog.

## Verification boundary

Focused root/KVM-free Go tests exercise local durable files and catalogs, exact MachineSpec resources, restart accounting, overflow-safe policy, publication/preparation deletion barriers, and real backend plus facade mTLS/gRPC listeners in separate trust domains. Only the launcher/VMM seams are fake in the local integration test. Other transport composition tests use `vmlibtest` as explicitly named. These tests prove source-level policy/transport behavior, not real Firecracker boot, installed cgroup enforcement, off-host access or deployment readiness. Existing early Delete tombstones, uncertain Prepare ownership, receipt checks and Close-drain barriers remain unchanged.
