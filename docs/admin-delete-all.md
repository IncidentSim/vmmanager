# Administrative VM snapshot deletion

`/delete_all` is disabled by default. This change adds a command; it does **not**
run cleanup on installation, startup, or configuration reload. No images,
committed images, or base disks are deleted by this command.

## Configuration and activation

Set `"admin_user_ids": [123456789]` explicitly in **both** the vmapi and vmbot
configuration, using the administrator's immutable numeric Telegram sender ID.
Omission or `[]` disables administrative access. Positive unique JSON integers
are required; usernames, display names and membership in `allowed_user_ids` or
`allowed_usernames` do not grant administration. Keep the list small. The lists
are copied at startup, not mutable through HTTP. This role is separate from
ordinary ownership access; an admin need not be on the ordinary allowlist.

The bot requires its existing per-user JWT signing configuration when admins
are configured (see `deployment/{vmapi,vmbot}/*.jwt.example.json`). The API
verifies the signed numeric `sub` using its public key and compares it to its
own configured admin IDs. Do not share admin personal `/token` bearers: such a
bearer intentionally carries administrative authority at the admin endpoints.
No caller-supplied `admin` flag is accepted. `X-Acting-User-ID` is forbidden for
JWT/user bearers; **service/delegation credentials cannot call admin routes**,
even if they select a configured administrator. Existing service-mode bots
must migrate to per-user JWT before enabling this command.

An operator must separately review this code, choose admin IDs, and activate
updated API/bot binaries/configuration in a maintenance window. Do not change
live config, restart services, or perform cleanup merely to validate this
feature. Old API binaries cannot read the additive admin journal after use;
back up state and use a compatible binary rather than dropping journal fields.

## Telegram flow

Only Telegram's verified sender in a private chat whose ID equals the sender
ID is admitted. Ordinary users do not see inventory or confirmation controls.

1. `/delete_all` persists a preview snapshot covering **all owners**, including
   revoked ordinary owners and retained VMs. The bot lists each VM ID/name and
   owner plus the total. Nothing is deleted yet.
2. The confirmation button carries `da:<43-character opaque random token>`.
   The server binds the token to the admin sender/chat, an exact ordered ID and
   owner snapshot, and a five-minute preview expiry. The token is not an auth
   credential. Forwarding a button to another sender/chat does not authorize it.
3. Confirmation durably records intent. Each subsequent advance admits at most
   one outstanding per-VM Delete operation. Later-created VMs are excluded.
   The server reads the original owner internally; clients cannot supply an
   acting owner. The existing safe owner workflow/DBOS Delete path is unchanged.
4. The bot waits only its normal wait budget, then reports actual completed,
   failed/retained and pending counts. The existing Telegram delivery polling
   loop advances confirmed snapshots in the background and sends final counts.
   Restart resumes the same journal, never a fresh inventory snapshot.

A repeated callback or HTTP delivery returns/advances the same transaction.
Every per-ID operation has the durable key
`admin-delete:<confirmation-token>:<immutable-vm-id>` and the normal owner
identity, with `admin_user_id` additionally persisted for audit. Failed or
uncertain deletion is never replaced by force-kill, direct backend calls, or
workspace/image deletion. Terminal failures remain in the report and are not
automatically retried; a new reviewed preview is required for another attempt.
Unknown outcomes remain pending, not falsely completed or failed.

Final-message receipts are fsynced in the bot's existing private delivery
journal (`admin_reports`). A crash after Telegram accepted the message but
before fsync may duplicate a summary, but cannot duplicate lifecycle effects.
The background loop requires the bot to run; if offline, already-admitted DBOS
operations continue and remaining snapshot IDs wait for the next advance.

## HTTP/SDK contract

All routes require a verified user principal in API `admin_user_ids`. Ordinary
routes retain their ownership checks and identical foreign/unknown 404s.

- `POST /v1/admin/delete-all`, `Idempotency-Key: <preview-key>`,
  body `{"chat_id":123456789}`: **202** snapshot. The preview key is scoped to
  the admin and always replays the original snapshot, even after expiry.
- `POST /v1/admin/delete-all/<token>`, body `{"chat_id":123456789}`: **202**
  confirm/advance the exact snapshot. First confirmation after expiry: **409**;
  unknown or another admin's token: **404**; nonadmin/service bearer: **403**.
  Confirmed transactions may continue beyond preview expiry.
- `GET /v1/admin/delete-all`: **200** `{"batches":[...]}` containing only the
  authenticated admin's confirmed snapshots, including completed ones for
  final-message recovery. This GET never admits deletion.

A snapshot has `token`, `admin_user_id`, `chat_id`, `key`, `expires_at`,
`confirmed`, `vms`, and `pending`, `completed`, `failed` counts. Each VM reference
has `id`, `name`, `owner_user_id`, and, after admission, `operation_id`, `status`
and optional `error_code`. Preview references have no operation yet. Zero-count
empty snapshots can be confirmed safely. Unknown JSON fields are refused.

`controlclient` exposes `AdminPreview`, `AdminAdvance`, `AdminBatches`,
`AdminBatch`, and `AdminVMRef`; it uses the existing user-specific bearer source.

## Offline verification

Use fake VM/Telegram backends and temporary state only:

```
GOMAXPROCS=2 go test -race -p 1 -timeout 240s ./internal/controlapi ./internal/tgbot ./controlclient ./internal/vmbot ./cmd/vmapi ./cmd/vmbot -count=1
```

Tests cover multi-owner snapshots, later creates, ordinary/foreign/group
refusals, signed subject mapping and delegation refusal, expiry/stale tokens,
restart and callback replay, per-ID audit identity, retained uncertain failures,
immutable journal validation, preserved base images, and durable delivery
receipts. These tests do not exercise a live host or prove live activation.
