# ADR 0002: Telegram single-VM bot with a capability-link web terminal

Status: Accepted; the single-VM occupancy rule is amended by [ADR 0003](0003-vmbot-multi-vm.md)
Date: 2026-11-10
Baseline: commit `76375b2` (verified multi-VM deployment of `vmlib`/`vmd`)

## Context

One operator wants to create, inspect, open a shell in and delete a throwaway VM
from a phone. `vmlib.Lifecycle` (served by `vmd` over mTLS) already owns every
safe lifecycle rule (receipts, retained disks, admission). The new application
must not re-implement any of it, must not change daemon policy, and must not
disturb the legacy builder or the multi-VM deployment.

## Decision

`cmd/vmbot` is an ordinary facade client (`remote.DialFacade` + mTLS) with three
parts in `internal/vmbot`:

1. a **Telegram front end** (stdlib HTTPS long polling, no framework);
2. a **controller** that enforces "exactly one occupied VM" and persists its
   identity durably;
3. a **terminal gateway** (HTTPS reverse proxy in front of one `ttyd` that runs a
   fixed `ssh` argument array) with a per-VM secret link that can be revoked and
   that actively disconnects open sessions.

There is no CRUD website, no VM TTL, no audit/summary feature and no custom
lifecycle daemon: `systemd --user` supervises the bot; `vmd` supervises VMs.

### Single occupied VM

"Occupied" means the application holds a VM in any non-deleted state, including
`stopped`, `failed` and `retained`. Create is refused when
(a) the durable state holds a VM, or (b) `List` on the shared facade returns any
VM (foreign VMs are never adopted, stopped or deleted), or (c) `List` fails
(fail closed). All Create/Delete/reconcile work is serialized by one lock.

### Durable state and identity

`state_dir` (0700) holds `state.json` (0600, written atomically: temp file,
fsync, rename, directory fsync) and `lock`, which carries an exclusive `flock`
for the process lifetime so two controllers cannot run. The state records the
monotonic **generation**, the VM ID (random `bot-<hex>`, never reused), the
terminal secret, the phase (`creating`, `active`, `deleting`) and the Telegram
update offset. Identity and secret are saved **before** `Create` is called, so a
crash or ambiguous RPC failure cannot orphan a VM. After an ambiguous error the
controller keeps the occupancy and resolves it with `Get`: found (even
failed/retained, or still `preparing`) -> adopted; any error -> still occupied,
retried by the next operation or restart.

**`NotFound` never releases the slot by itself.** A Create RPC that timed out may
still be running on the daemon, or arrive late. The only release proof is a
successful `Lifecycle.Delete`, which in `vmlib.Service` is a per-VM barrier: it
waits on the VM's operation lock until an in-flight Create has settled (the
"nothing known" decision is re-checked under the same lock Create uses to insert,
so a Create racing the lookup is never missed), then frees or reports it. Delete
**intent** (not just success) consumes the ID **for the lifetime of that Service**
(a non-expiring in-memory tombstone taken atomically at the start of Delete, even if
that Delete later fails or is cancelled), so a delayed, retried or successor Create
for it is refused (`ErrAlreadyExists`) and Delete can never act on a stale record
that a same-ID replacement outlived. VM IDs are therefore single-use per Service
instance; the bot generates fresh random IDs. Consequently:

* `Get` on a VM whose Create is unsettled, or whose identity is retained, returns
  it (`preparing`/retained), never `NotFound`.
* An ambiguous `Prepare` failure (transport error) is released only if the backend
  actually reports that VM as prepared. `Inspect` NotFound or a `Release` that says
  "already released" is **not** proof, because the backend request may still be
  running (the existing protocol has no cancellation barrier). Otherwise the record
  stays retained, `Delete` keeps answering `ErrRetained`, and the slot stays
  occupied.
* Unresolved `creating` or `deleting` slots are re-resolved on every operation and
  at startup by the same barrier; if it cannot settle (timeout, error, retained) the
  slot stays occupied. Resolving an unconfirmed creating slot via the barrier
  deletes whatever that Create produced; it was never shown to the user.
* Delete persists the irreversible `deleting` phase **first**, then revokes the
  terminal, then calls the daemon. If the phase cannot be persisted nothing is
  revoked or deleted and the user is told so; a crash at any later point restarts in
  `deleting`, where the terminal is never restarted.

Irreducible uncertainty and operator recovery: the tombstone set and retained
records are in memory, so they do not survive a `vmd` restart; and a backend that
is still running an orphaned Prepare cannot be cancelled by `vmlib`. For an
ambiguous Prepare the operator must verify on the backend host (list VMs/workspaces,
look for the ID) and remove anything found, then restart `vmd` to drop the retained
record; only then can the bot's `/delete` succeed (Delete of an unknown ID is
`Deleted`). Until then the bot stays occupied and refuses new VMs.

### Telegram

Only `message`/`callback_query` from the configured user ID in a private chat
whose ID equals that user are processed; everything else is dropped silently. The
token is read from a separate 0600 file and exists only in the request path of
the Bot API call; transport errors are reduced to a stage name so neither the
token nor RPC internals reach logs or chat. The poll offset is persisted **before**
an update is handled (at-most-once): a crash can lose a command but never replays
a delete or create. Inline buttons carry the generation and a VM ID suffix, so a
stale delete button cannot delete a replacement VM. `/delete` typed by the user
is explicit and acts on the current VM. The link message disables link previews
and protects content from forwarding.

### Terminal authorization

The user chose a random reusable secret link per VM session:
`PUBLIC_ORIGIN/t/<256-bit crypto/rand token, base64url>/`.

* Valid from creation until that VM is deleted; usable any number of times.
* Delete revokes first, then deletes the VM. Revocation removes the token,
  closes every tracked hijacked (WebSocket) connection, kills `ttyd` and the SSH
  process groups beneath it, and deletes the per-VM `known_hosts`. A replacement
  VM has a new ID, token and `known_hosts`. A failed delete leaves the VM
  occupied but its terminal stays revoked until it is actually deleted.
* Every request (page, resources, WebSocket) must carry the token in the path and
  an exact `Host`/`Origin` match with the configured `public_origin`; forwarded
  headers are never trusted. Any other path, a query string, or a method other
  than GET/HEAD is rejected before reaching `ttyd`, so URL arguments can never
  override the command. All answers carry `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`, a restrictive CSP and `nosniff`; nothing is
  fetched from third parties and the gateway logs no request URL.
* Opaque fragment plus secure cookie would hide the token from server logs of an
  intermediary. It was not chosen to avoid a second state machine; operators who
  front the bot with their own proxy must disable URL logging there.
* Token comparison is constant time; failures are indistinguishable (404).

### Backend process

`ttyd` listens on a **unix socket** inside the 0700 state directory (so other
local users cannot bypass the gateway, which is stronger than loopback TCP), with
`--writable --check-origin --max-clients N`; `--url-arg` is never passed, there
is no `--base-path` (the gateway strips the secret prefix; the secret never
appears in `ttyd`'s own argv or paths) and no shell. It executes this fixed argv
(no user value, no shell): `ssh -F /dev/null` with `BatchMode=yes`,
`IdentitiesOnly=yes`, `IdentityAgent=none`, `ForwardAgent=no`, `ForwardX11=no`,
`ClearAllForwardings=yes`, `StrictHostKeyChecking=accept-new`, a per-VM
`UserKnownHostsFile`, the configured key, and `user@ip`. IP comes from `SSHInfo`
and must parse as an IP; user must match a conservative pattern. The advertised
bastion is ignored unless `ssh_use_bastion` is set (direct on the reference host).
Start fails loudly if the socket does not come up, so an unsupported `ttyd`
flag set is detected immediately.

### Resource bounds

One `ttyd`, `max_clients` WebSockets (enforced by gateway and `ttyd`), bounded
connection count, header/read/write/idle timeouts compatible with hijacked
WebSockets, and `TasksMax`/`MemoryMax` in the user unit. `ttyd` runs each client
in its own session, so a process-group kill alone would miss `ssh`; revocation
enumerates descendants via `/proc` and signals their groups, with systemd
`KillMode=control-group` as backstop. Process exit never deletes the VM.

## Consequences

* The link is a bearer capability to a guest shell. Whoever has it has the shell
  until the VM is deleted; Telegram's servers see it. This is the chosen trade-off.
* Proxy protection is only as strong as TLS to the gateway; HTTP is accepted only
  with an explicit test flag on a loopback listener.
* A crash between persisting the poll offset and acting loses that command.
* Real `ttyd`, Telegram and VM behaviour are proven only by the live procedure in
  `docs/vmbot.md`; offline tests use a fake `ttyd` and a fake Bot API.
