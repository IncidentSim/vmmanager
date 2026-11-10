# HTTP-only Telegram frontend and Go SDK

`cmd/vmbot` is now a separate Telegram presentation/delivery process using
`controlclient` over HTTP. It does **not** import the legacy controller, DBOS,
gRPC, the facade, or terminal manager in production. The control API owns VM,
image, user, quota and workflow state. API downtime is an error, never a reason
to start an embedded controller. See `control-api.md` for server setup.

## Configuration

Use `deployment/vmbot/vmbot.example.json` as a **new** config, not as an in-place
replacement for a deployed legacy config. JSON is strict: old `state_dir`,
`allowed_user_id`, `facade`, `terminal`, `vm`, `max_vms` and unknown fields are
rejected. `version: 1` is the frontend config schema (not an API state version).

Required fields: `version`, `telegram_token_file`, `api_url`, `api_token_file`,
`allowed_user_ids`, `delivery_dir`, `image_id`. `resources` is optional; omit it
to use the API's default policy. When supplied, both positive `vcpu` and
`memory_mib` are required. The API/backend remains authoritative for admission;
the bot does not clamp RAM, CPU, or quota. Disk resizing is not implied.

* Config and both secret files must be regular files owned by the frontend uid,
  with no group/other permissions (normally 0600). Final-component symlinks are
  refused. Credentials contain one nonempty token, not JSON or an inline CLI
  secret. Trusted private parent directories should prevent path replacement.
* `allowed_user_ids` must be a nonempty list of unique positive Telegram numeric
  IDs. The API must separately delegate the service credential for those users.
  Sender ID must equal private-chat ID; groups, mismatched identities, missing
  senders and unknown users are dropped without a reply or API call.
* `api_token_file` holds a **bot service** credential, not an ordinary user token.
  `Authorization: Bearer …` plus `X-Acting-User-ID` uses the verified Telegram
  sender; callback contents never supply identity. Removed API users stay denied
  even if an obsolete frontend config still lists them.
* API URLs must use HTTPS, except literal loopback IPs or `localhost` may use
  HTTP. URL credentials/query/fragment/path prefixes and all redirects are
  refused. Use a root-mounted trusted TLS endpoint for a remote API. Public
  Telegram's URL is fixed; config cannot redirect the Telegram token elsewhere.
* `delivery_dir` is an absolute path to a distinct owned private directory
  (0700), **not** the API state or legacy bot directory. It contains only a
  `delivery.lock` lifetime flock and private `delivery.json` offset. Legacy/API
  `state.json` and known DBOS files are refused there. Use a dedicated directory,
  not a subdirectory alias or bind mount of API state.

Validate without network access (using only your new private config):

```sh
vmbot-http -config /path/to/new-vmbot.json -check-config
```

## Safe one-time legacy handoff

No command below is performed automatically. Keep existing deployed binaries,
config and state untouched until an operator has planned the API migration.
This release changes `cmd/vmbot` behavior incompatibly; do **not** overwrite a
running legacy bot's binary or reuse its service unit/config blindly.

1. Build/install the new executable at a **different** path (`vmbot-http`) and
   prepare the new config/credential files privately. Preserve the old binary,
   config and exact state backups. Coordinate API state owner migration with an
   explicitly configured `legacy_owner_user_id`; offset import does not migrate
   VM ownership or touch DBOS state.
2. Stop **and disable** the legacy Telegram poller. Verify it is no longer
   running. Never have two `getUpdates` consumers for the same Telegram token.
   Leave the original state file and its existing `lock` file in place. Do not
   restore an older snapshot after newer commands were accepted.
3. Before starting an API process that might hold the legacy state lock, run
   this one-time import into an **unused** new delivery directory:

   ```sh
   vmbot-http -config /path/to/new-vmbot.json \
     -import-legacy-offset /path/to/stopped-legacy/state.json -legacy-stopped
   ```

   The explicit flag is an operator attestation, not proof that systemd is
   stopped. The importer additionally takes the existing legacy `lock` with a
   nonblocking exclusive flock, opens the named state file read-only without
   following a final symlink, requires owned/private regular files, accepts
   only version 1 or 2 with an explicit nonnegative `offset`, and durably
   installs that exact next-update offset. It never copies VM records, terminal
   tokens or DBOS files; it never changes the source's contents. A live lock,
   missing lock, malformed/public source, repeated import or already-used new
   delivery state is an error. The command exits without contacting Telegram.
   If API state has already migrated to v3, use the preserved exact v1/v2 source
   and its lock only after independently verifying the offset is the final
   legacy high-water mark. Never import a stale backup just because it parses.
4. Start/configure the API using its separate documented migration procedure,
   check authorization, then start only the new frontend with the same Telegram
   token. The unit template uses the new executable/config paths and must be
   installed under a distinct name such as `vmbot-http.service`.
5. Rollback is **not** simply restarting the old poller: after new updates were
   consumed its old offset is stale and can replay mutations. Stop the frontend
   first and make an explicit operator plan preserving the newest offset and
   API-owned resource state. Never run the old controller against new API state.

A genuinely new Telegram token can start with a fresh delivery directory at
offset zero. An existing token must complete the handoff first. There is no
heuristic that can infer from a new directory whether a token has old commands;
starting it without import can replay old updates.

## Delivery guarantees and command behavior

The high-water mark is written to a private temp file, fsynced, atomically
renamed, and directory-fsynced **before handling** each update. Persistence
failure stops polling. Updates below that offset are ignored. This intentionally
preserves the legacy **at-most-once acceptance** policy: a crash after offset
persistence and before API admission can lose a command. A crash before sending
the Telegram reply can lose the reply. This is **not exactly-once delivery** and
there is no durable Telegram outbox. There are no automatic message-send retries.
API idempotency remains separate: a mutation uses `tg:<update_id>:<action>`;
retrying the same update/action uses the same key. A new user command is a new
intent and gets a new key. API HTTP requests never retry mutations internally.

The Russian main keyboard and `/start`, `/help`, `/create [имя]`, `/list`,
`/status [имя]`, `/terminal [имя]`, `/delete [имя]` remain. A lone owned VM is the
default; multiple matching VMs require an explicit name/ID or button. No global
or foreign VM is chosen. `/list` sends every result across bounded pages of at
most 8 rows and 3500 characters. Buttons contain the action and SHA256 digest of
the immutable API VM ID (45 bytes), not a user ID/name/reusable generation.
On every press the bot resolves that digest against the verified user's current
owned list. Stale, removed, foreign and legacy generation buttons cannot target
a replacement VM. Resource authorization is checked again by the API.

Additions:

* `/create имя image_id [vcpu memory_mib]` selects a visible image and optionally
  explicit resources; `/create` and `/create имя` use configured defaults.
* `/stop [имя]`, `/commit имя-VM имя-нового-образа`, `/images`,
  `/deleteimage image_id`, `/ssh [имя]`, `/capabilities`.
* `/operation operation_id` retrieves a caller-owned result if polling outlasted
  the caller's budget. Operations are not marked failed just because polling
  timed out. Successful creation automatically requests a terminal; if creation
  completes after the bot's budget, use `/terminal имя` after checking status.
  Successful async commit/delete image responses expose their resource IDs via
  `/operation`; `/images` also lists published images.

Each API request has a maximum 30-second timeout, 4 MiB response bound, fixed
safe errors, and no redirects. Bot operation waits are capped at 40 seconds;
SDK waits are capped at one minute or an earlier caller deadline. Polling
continues to expose the last observed server state, never fabricated success.
The current implementation processes commands sequentially; long operations can
delay subsequent updates. Raw API messages, transport URLs, credentials and
terminal links are not logged. SSH output is public connection metadata only;
it never distributes private keys. API/backend may decline caller SSH keys;
no support is inferred from a field existing in the SDK.

Terminal messages disable link previews and set Telegram `protect_content`.
They explicitly warn that the URL is a bearer capability: someone receiving a
copy can use shell access. Telegram protection is not identity-bound guest
login and cannot prevent screenshots or manually copied links. Stop, cold
commit and delete revoke terminal access server-side. Bot allowlisting and
per-owner API state **are not guest-network isolation**.

## Reusable Go client

```go
client, err := controlclient.New(apiURL, serviceToken, nil) // delegated bot service
// Alternatively: NewUser(apiURL, userToken, positiveUserID, nil) never sends an acting header.
a, err := client.Create(ctx, verifiedUserID, stableIntentKey, controlclient.CreateRequest{
    Name: "lab", ImageID: "ubuntu",
    Resources: &controlclient.Resources{VCPU: 1, MemoryMiB: 512},
})
op, err := client.Wait(ctx, verifiedUserID, a.OperationID, time.Second)
```

Check every `err` (the snippet omits checks for brevity). The SDK also exposes
VM/list, stop, commit, delete, terminal, SSH, image/list/delete, capabilities,
and operation reads. IDs are URL-path-escaped. Errors retain safe API codes and
HTTP status via `*controlclient.APIError`, but discard server free text.
Clients are reusable concurrently; caller contexts bound requests. No external
library other than the Go standard library is required by `controlclient`.

## Usernames, `/token` and per-user JWT

With `allowed_usernames` and `jwt` (see
[username-jwt-swagger.md](username-jwt-swagger.md)) the bot admits a sender only
when Telegram's own `from.username` (case-insensitive) is listed, the chat is
private with chat ID equal to the sender ID, and the slot is or becomes pinned to
that numeric ID. Every API call is made with a short per-user RS256 token; there
is no service credential or `X-Acting-User-ID`. `/token` returns a personal token
(default 28800 s) as a protected code block without link preview; it is never
logged. `controlclient.NewPerUser` is the SDK constructor for this mode.

## Verification scope

Focused tests cover real HTTP envelopes and real control API/controller/DBOS
SQLite with `vmlibtest` fake remote host and fake Telegram, including explicit
resources, per-owner names, foreign reads/buttons, stable-key conflict/replay,
terminal creation, stop/commit/delete/image deletion. Transport tests separately
exercise Telegram Bot API JSON flags and token-safe errors. Offset tests cover
v1/v2 import, lifetime locks, permissions, symlinks, read-only source preservation,
restart, monotonicity and fail-before-dispatch. These are root/KVM-free tests,
not evidence of live Telegram delivery, a real VM boot, real terminal processes,
network isolation, production migration or a deployed service.

```sh
GOMAXPROCS=2 go test ./controlclient ./internal/tgbot ./cmd/vmbot
GOMAXPROCS=2 go test -race ./controlclient ./internal/tgbot ./cmd/vmbot
```
