# Unified browser SSH through Nexterm

`vmapi` is the sole owner of Nexterm provisioning, the durable binding store and
admin credentials. `vmbot` is an HTTP-only client: it has no Nexterm config,
credentials, provisioning hook, poller or separate `/nexterm` command.

## API and bot contract

With Nexterm enabled, `GET /v1/vms/{id}/ssh` retains the raw SSH contract
(`host`, `port`, `user`, `host_key_fingerprint`, `ready`, `auth_mode`,
`key_reference`, optional `bastion`) and adds:

* `browser_status: "ready"` and `browser_url` when the link is available;
* `browser_status: "pending"`, without a URL, while provisioning/session validation
  is unavailable; raw SSH is unaffected;
* `browser_status: "unsupported"`, without a URL, for an owner-supplied public key
  (or a legacy VM without an API-retrievable generated key). Raw SSH still works.

The existing `POST /v1/vms/{id}/terminal` operation protocol remains available.
A successful operation's result has `browser_status` and, when ready, `url`
(the same link as `/ssh.browser_url`). Pending/unsupported results contain no
URL. A completed operation can be polled again to retry link preparation.

The API proves ownership before producing a link, then the Nexterm manager
re-proves VM ownership/liveness using the **in-process** controller and encrypted
key provider. There is no loopback HTTP request or synthetic administrator owner.
No browser terminal request starts ttyd when Nexterm is enabled, including
supplied-key VMs: **there is no ttyd fallback**. Existing terminal gateway config
may be retained for the authenticated API/Swagger routes; its ttyd manager is
not the controller's terminal provider. Omit `terminal` and disable
`expose_api_on_terminal` when that gateway is unnecessary.

The bot's existing `/create` -> terminal-link flow, session-menu Terminal button,
`/terminal` alias and `/ssh` all use the API link. `/ssh` also retains the raw
SSH connection details. Delivery is only to the admitted owner's private chat,
as ordinary copyable messages (not protected content). No additional
login/password message is sent. Pending tells the owner to retry `/terminal`;
there is no unsolicited credential notification later.

## Activation configuration (operator action, not deployment performed here)

Put this section in **vmapi** JSON, not vmbot JSON. Complete fragment example:
`deployment/vmapi/nexterm.example.json`.

```json
"nexterm": {
  "enabled": true,
  "url": "https://nexterm.example.org:19454",
  "admin_credentials_file": "/var/lib/vmapi/private/nexterm-admin.json",
  "state_file": "/var/lib/vmapi/private/nexterm-state.json"
}
```

* Omitted section (or only `{"enabled":false}`) preserves legacy terminal behavior.
  Disabled config must not retain the other settings.
* `url` is a bare HTTPS origin (HTTP allowed only on loopback for tests).
* Private paths must be absolute, clean and different. Admin file is owned by
  the API UID, mode 0600, a regular file, not a symlink, containing
  `{"username":"...","password":"..."}`. Admin credentials are validated at
  startup without network access; an unreachable Nexterm does not block startup.
* State is an owned 0600 regular file in an owned 0700 directory, not a symlink.
  `<state_file>.lock` is an exclusive process-lifetime lock. The API UID must own
  the parent and files; service sandbox read/write paths must allow both files
  and the lock. The bot needs no access to any of them.
* Generated-key browser access requires the backend's provision-public-key mode
  and its existing encrypted credential store/master key. Do not invent or copy
  an SSH private key into config. Nexterm must reach the returned raw SSH
  host/port; this integration does not use a bastion to reach the guest.
* Activate a frontend build that consumes both `connectId` and `connectFtpId`
  before expecting automatic terminal + file-manager opening. Building this API
  does not update the separately deployed Nexterm frontend.

### Migration from the old bot-owned store

These are requirements for a later operator-controlled maintenance window.
No protected existing configuration/state/admin files need to be read to prepare
this change, and none were read or modified during implementation.

1. Stop the old bot **before** starting a Nexterm-enabled API against its state
   path. Confirm the old bot exited and released the store lock; never bypass or
   delete an active lock. Stop/restart the API as needed for config activation.
2. Back up the existing private Nexterm state/admin files without printing their
   contents. Preserve the complete state, including account IDs, generated
   credentials and `link_token`. Do not start an empty store for existing accounts:
   doing so loses cleanup ownership and can provision duplicate accounts.
3. Either keep the exact existing paths in the API's `nexterm` section, transferring
   owned-directory/file/lock ownership and service sandbox access to the API UID,
   or move the entire state/admin files to an API-private 0700 directory while
   both processes are stopped. Preserve 0600 modes and ensure no symlinks.
   Reusing a bot-private path requires adjusting its parent ownership too.
4. Remove the entire `nexterm` section from the bot JSON (the strict bot config
   rejects it). The former `deployment/vmbot/nexterm.example.json` is obsolete;
   use the vmapi example. Do not change Telegram delivery offsets or JWT ownership.
5. Start the API first. It alone opens and locks the store, re-tracks the durable
   controller VM snapshot and starts reconciliation/retry. Then start the new
   HTTP-only bot. Verify an owner can get one copyable browser link through
   `/ssh`, `/terminal`, the menu and a new `/create`; verify another owner gets 404.
6. Validate generated-key terminal + SFTP in the updated frontend, explicit
   unsupported browser status for supplied-key VMs, and account/session revocation
   after delete/TTL. These are live checks, not claims made by the mock tests.

Do not run the old and new Nexterm owners concurrently, even against copied
stores: copied stores can race cleanup and session ownership.

## Durable lifecycle and security

API admission of create/terminal durably tracks provisioning intent. Create
replays re-track it. Startup re-tracks the authoritative controller VM snapshot,
repairing the crash window between admission and the secondary store write.
Direct `/ssh` access also provisions lazily. Random per-VM account credentials
are persisted before any Nexterm write; restart resumes the same account,
identity and entry. Only the generated owner key is used; the private key is
not persisted in the Nexterm binding store (Nexterm holds its personal identity).

A background loop runs immediately at startup and every 30 seconds, with a
25-second batch deadline. Pending retries are capped at three per batch and use
30-second to 16-minute exponential backoff. Cleanup runs at most every five
minutes, capped at five bindings per pass, before retries. Expired/deleting VMs
are eligible immediately at the next cleanup pass. Missing VMs require both a
successful owner list without the VM and an owner lookup returning `not_found`.
Transient backend/auth/list failures retain bindings rather than deleting an
unproven account. Removed allowlist owners therefore need operator attention;
cleanup never impersonates an unadmitted owner. API shutdown cancels and joins
the loop before closing the store.

The link is:

```text
<origin>/servers?token=USER_SESSION&connectId=ENTRY_ID&connectFtpId=ENTRY_ID
```

The token is a per-VM ordinary user's session, never the admin session. Repeated
requests reuse it after exact account ID/username/non-admin and entry-readability
validation. A rejected/dead session is replaced; transient failures do not mint
sessions. Admin and short-lived provisioning sessions are closed individually.
`POST /api/auth/logout` (global account logout) is never used. Cleanup verifies
recorded account ownership, revokes the stored link session, deletes the personal
entry/identity/account and confirms account absence before forgetting the binding.
No unrelated account is touched.

Links are bearer credentials in URL query strings and forwardable messages.
Do not log API response bodies or Nexterm URL queries; configure proxies and
analytics accordingly. The store contains generated account passwords and link
tokens, not SSH private keys. A crash between impersonation and token persistence
can leave an unreferenced session until Nexterm expires/revokes it. The API does
not revoke every browser session just because a VM stops; delete/TTL cleanup
revokes its account. Nexterm outages defer deletion until reconciliation succeeds.

## Verification and rollback

The handler/entry-point tests use real HTTP, API authentication, owner-scoped
controller/SQLite operations and a fake Nexterm HTTP server; no live guest,
Nexterm engine or deployed frontend is exercised:

```sh
GOMAXPROCS=2 go test -race -p 1 ./internal/controlapi ./internal/nextermaccess \
  ./internal/tgbot ./controlclient ./cmd/vmapi ./cmd/vmbot
```

`e2e/TestLiveNexterm` remains a legacy opt-in low-level manager/engine test,
not validation of the unified API/bot flow; it must not open the production
API-owned store. No live test was run for this implementation.

For rollback, stop the new API before transferring store ownership back. Restore
compatible old bot/API binaries/config together if returning lifecycle ownership
to the bot. Simply disabling Nexterm on the API restores legacy terminal behavior
but **does not delete existing Nexterm accounts**; keep the state for controlled
cleanup or later re-enable. Never discard it as a rollback shortcut.
