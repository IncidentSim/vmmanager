# Common image registry

The image registry is common to all admitted API users. Any **ready** image committed by an admitted user can be listed, fetched and used to create another user's VM. This also applies to existing creator-owned image records: no IDs, names or creators are rewritten when enabling this behavior.

VMs, operations, SSH information and terminal capabilities remain owner-scoped. Sharing an image does not reveal or grant control over its source VM.

## Permissions and state

Image responses contain:
- `id`: immutable image identity used for booting and deleting.
- `name`: human-readable label.
- `shared`: true for entries in the common catalog.
- `phase`: `ready` or `deleting` for visible entries. Publishing images are not listed or bootable. A deleting image remains visible only to its creator so failed deletion can be inspected and retried; it cannot be booted by anyone.
- `can_delete`: whether this caller has deletion permission. Only the creator has permission; configured system images are protected, even if they also have a creator record. This is **not** a promise that deletion will succeed now.

The durable creator ID remains unchanged in internal metadata; public image listings do not expose other users' numeric identities. The bot uses `can_delete`, rather than `shared`, to show creator-only deletion actions.

The API rejects deletion while **any** VM references an image, regardless of owner. Pending create admissions reserve the reference too. Backend checks remain the final guard against unsafe filesystem deletion. Once references are removed, the creator can retry deletion. After a terminal transient deletion failure, a new idempotency key starts a fresh attempt; replaying the original key returns the original operation. Neither retry nor shared visibility bypasses publishing/deleting boot fences.

## Names and legacy records

New commits reserve names globally across all creator records (publishing, ready and deleting) and configured system image names. Concurrent attempts to reserve the same name have one winner. Original operation replay is resolved before name reservation checks.

Old duplicate names from formerly private catalogs remain valid: their IDs and creator metadata are preserved, and both ready entries remain listed. Use the immutable ID to disambiguate them. Records disappear only after successful deletion; ambiguous publication remains fail-closed and requires operator recovery rather than automatic identity reuse.

## Disk contents are shared too

A committed image contains its disk contents, not just installed packages. Files, configuration, passwords and other secrets deliberately left in that disk become available to any admitted user who boots the image. The registry does not sanitize images. Host-side Telegram/JWT keys are not injected into guest disks by this feature.

## Verification

Domain tests cover two synthetic users sharing an image while retaining VM/operation/SSH/terminal isolation, creator-only deletion, cross-owner pending/running references, deleting-image boot refusal, retry semantics, protected system images, global name races and preservation of old IDs and duplicate names. HTTP and bot tests cover `shared`, `phase` and caller-specific `can_delete` presentation. A configured system image overriding a creator record has exactly one catalog entry and never offers deletion.
