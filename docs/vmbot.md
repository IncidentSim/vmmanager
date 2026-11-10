# vmbot: Telegram multi-VM bot

Decisions: [ADR 0002](adr/0002-telegram-single-vm-bot.md) (base design) and
[ADR 0003](adr/0003-vmbot-multi-vm.md) (several VMs, state v2, per-VM isolation;
it supersedes every "exactly one VM" statement below and in ADR 0002). `vmbot` is an ordinary
legacy direct client of the since-removed `vmd` facade (historical; the bot is now an HTTP-only API client, see `bot-api-client.md`); it does not change daemon
policy and does not touch the legacy builder or the multi-VM deployment.

The multi-VM/DBOS variant is not activated as the production bot. Its controller,
gateway and real guests passed the bounded host smoke test in
[DBOS verification](vmbot-dbos-verification.md); distinguish that evidence from
[offline verification boundaries](#tests-and-verification-boundaries).

The current orchestration design is [ADR 0004](adr/0004-vmbot-dbos-orchestration.md):
DBOS Transact Go v1.8.0, embedded pure-Go SQLite under
`<state_dir>/orchestration/dbos.db`. There is no workflow server or broker.
Builds require Go 1.25+; Make selects the matching child toolchain for coverage
without modifying the system Go installation. Back up the whole private state
directory while stopped, including JSON domain state and the SQLite database
(and WAL if present). Never delete the database to repair a pending operation.

## UX (Russian, private chat only)

| Command / button | Effect |
|---|---|
| `/start`, `/help` | help and the persistent keyboard (Создать, Список, Статус, Терминал, Удалить) |
| `/create [name]` | create another VM until `max_vms` or backend capacity; no name = short label `vm-<generation>`; success sends the status and then that VM's secret terminal link automatically |
| `/list` | every owned VM with status and per-VM *Терминал* / *Удалить* buttons (split over several messages if long; none is truncated) |
| `/status [name\|id]` | state of one VM; with several VMs and no selector it is the list |
| `/terminal [name\|id]` | the secret terminal link (previews off, forwarding protected) |
| `/delete [name\|id]` | revoke that VM's terminal, then delete that VM |

Names are immutable, unique, lowercase ASCII (`[a-z]([a-z0-9-]{0,18}[a-z0-9])?`,
not starting with `bot-`), display/selection data only: never a path, socket or
argv element. Selection is by exact name or exact VM ID. With exactly one VM the
selector may be omitted; with several, the bot shows the list and does nothing
(never "latest", never "all").

Inline buttons are `s:<gen>`, `t:<gen>`, `d:<gen>` (<= 22 bytes of the 64 allowed).
A generation names one VM forever: a button for a deleted VM answers "Устарело" and
cannot reach a replacement (even one with the same name). `d:<gen>` from before the
upgrade still selects the migrated original. Legacy `s`/`t` act only when
unambiguous. Callbacks are acknowledged before any slow work. Only the configured
Telegram user ID in a private chat with itself is served; any other sender, group or
callback is dropped without a reply.

## Owned VMs and capacity

* A record in any phase and remote state (creating, stopped, failed, retained,
  uncertain, deleting) occupies one of the `max_vms` slots (config, default 2, 1..8).
* The backend's own policy stays authoritative (reference capacity 2) and is not
  raised. Foreign VMs are never adopted, listed as ours, stopped or deleted; they
  consume backend capacity, and a backend refusal is reported as "no free resources".
  Create no longer consults `List`.
* DBOS queue partitions serialize lifecycle work by VM ID; different VMs
  create/delete/open terminals independently. Queue concurrency does not promise
  FIFO: durable phase checks enforce command causality. Admission (capacity,
  unique name, generation) is one atomic state
  update, so concurrent creates cannot overbook or duplicate a name.
* A failed or unconfirmed delete keeps that VM's slot; run `/delete <name>` again.
  Only a successful `vmlib` Delete (a settle-and-block barrier) releases it; a `Get`
  `NotFound` never does (a slow or late Create may still appear).
* No TTL, summaries or audit feature exist; `vmlib` keeps all safe lifecycle rules
  (retained disks, exit receipts).

## Terminal link

`https://PUBLIC_ORIGIN/t/<43-char token>/` — 256 bits from `crypto/rand`, one per VM
session.

* Reusable any number of times **until that VM is deleted**; not one-use, not a
  password. A replacement VM has a new ID, token and `known_hosts`; stale links 404.
* `/delete` first revokes: closes open WebSockets (tracked hijacked connections;
  `Shutdown` alone would not), kills `ttyd` and every `ssh` below it (each runs in
  its own session, found through `/proc`), deletes the per-VM `known_hosts`. Then the
  VM is deleted. After a delete attempt the terminal stays revoked even if the
  deletion fails; only a new VM gets a new link.
* Every request (page, `token`, `ws`) must carry the token in the path, an exact
  `Host`, and an exact `Origin` (required on WebSockets) equal to `public_origin`.
  `X-Forwarded-*` is never read. Queries, non-GET/HEAD methods and any path outside
  `/`, `/token`, `/ws`, `/auth_token.js` are rejected, so URL arguments cannot
  override the ssh command. Failures are indistinguishable (404).
* Responses: `Cache-Control: no-store`, `Referrer-Policy: no-referrer`,
  `X-Frame-Options: DENY`, `nosniff`, a CSP without third parties (ttyd's page needs
  `'unsafe-inline'`). The gateway logs no request lines. Telegram still stores the
  link text server-side; the link is a bearer capability for a guest shell. If you
  put your own proxy in front, turn its URL logging off.
* Not done on purpose: opaque fragment + cookie (second state machine).

### Backend process

`ttyd --writable --check-origin --max-clients N --interface <state>/ttyd-<vm>.sock
ssh ...` — a unix socket in the 0700 state directory (no TCP port, so other local
users cannot reach the raw backend), no `--url-arg`, no `--base-path` (the gateway
strips the secret prefix, so the secret never reaches ttyd), no `--once`, no
credentials in argv. The ssh argv is fixed:

```
ssh -F /dev/null -o BatchMode=yes -o IdentitiesOnly=yes -o IdentityAgent=none
    -o ForwardAgent=no -o ForwardX11=no -o ClearAllForwardings=yes
    -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=<state>/known_hosts/<vm>
    -o GlobalKnownHostsFile=/dev/null ... -i <ssh_key_file> -p 22 USER@GUEST_IP
```

`GUEST_IP` must parse as an IP (from `SSHInfo`), `USER` and an optional jump host must
match strict patterns. The advertised bastion is ignored unless
`"ssh_use_bastion": true` (the reference host reaches guests directly). There is no
shell anywhere in the chain.

Each VM has its own `ttyd`, unix socket and `known_hosts`. Deleting VM A revokes
only A (its ttyd, ssh descendants and tracked WebSockets); B's open connections and
URL keep working. `Close` (process exit) terminates every terminal and deletes no VM.
Slow starts and terminations never run under the manager lock; a start in flight is
cancelled by `Revoke`/`Close`. A token cannot be shared by two VMs.

### Bounds

At most `max_vms` `ttyd` processes, `max_clients` WebSockets per VM (gateway 503 +
`ttyd --max-clients`), `max_vms * max_clients <= 64` (enforced at config load), at
most 64 open gateway connections, header/read/write/idle timeouts,
`MemoryMax`/`TasksMax`.

## State and recovery

`state_dir` (0700): `state.json` (0600, atomic temp+fsync+rename, schema v2),
`lock` (exclusive `flock` for the process lifetime), `known_hosts/`,
`ttyd-<vm>.sock`, `orchestration/dbos.db` (SQLite execution log), and after an
upgrade `state.json.v1.bak` (0600).

### Upgrade from v1 and rollback

On first start `OpenStore` validates the v1 file, writes its exact bytes to
`state.json.v1.bak` (never overwriting an earlier backup; a different v1 gets
`.1`, `.2`, ...), then atomically installs v2. The existing VM keeps its ID,
token (link), generation, phase and the update offset; it gets the label
`vm-<generation>`. Corrupt, invalid (duplicate IDs/tokens/generations/labels,
bad phase/ID, generation above the high-water mark) or future-version state
makes the process refuse to start and is never modified. No backup possible = no
migration.

Rollback limits: v1 binaries refuse v2. Restoring the backup is safe only before
the first v2 write. Afterwards it loses the advanced update offset (commands may
be replayed), forgets VMs created since (still running and consuming capacity) and
generation history. Rolling back after extra VMs exist means deleting them first
and restoring manually with the bot stopped.

| Situation | Behaviour |
|---|---|
| crash/restart | VM kept; startup reconcile with `Get`; running VM keeps the same link and `ttyd` is restarted |
| crash during Create | identity+token were saved first; `Get` adopts the VM (even failed/retained/preparing); `NotFound` is resolved only by the Delete barrier, else the slot stays `creating`; an unprovable ambiguous Prepare stays retained until the operator verifies the backend and restarts `vmd` |
| crash during Delete | the `deleting` phase is persisted before the revoke, so the old link never returns; Delete is resumed |
| cannot persist state | Delete changes nothing (no revoke, no remote call) and says so |
| Create/Delete RPC timeout | occupancy kept; guarded reconciliation resolves it; identity never reused; another VM still requires its own free slot |
| remote unreachable | fail closed: no new VM, existing one not abandoned; next operation retries |
| two bots on one state dir | second one exits (`flock`) |
| corrupt/loose-permission state or token file | refuses to start |
| process exit | terminals closed, VM **not** deleted |
| Telegram updates | offset persisted **before** handling: a crash can lose one command but never replays a create/delete |

## Installation prerequisites (not performed)

* `ttyd` 1.7.x (`apt install ttyd` on Ubuntu gives 1.7.4) and OpenSSH client on the bot host.
* mTLS client material for the facade (CA, client cert/key) and a private SSH key the
  guest image trusts, all readable only by the bot user. Pass the Bot API token in its
  own 0600 file (`telegram_token_file`); never on a command line.
* Public HTTPS: either `tls_cert_file`/`tls_key_file` (built-in listener on its own
  port) or a loopback `listen_addr` behind an existing HTTPS proxy. Plain HTTP is
  only for local tests with `insecure_http_for_testing` on a loopback listener.
* Config: copy `deployment/vmbot/vmbot.example.json` (placeholders only) to
  `~/.config/vmbot/vmbot.json`. Unit: `deployment/vmbot/vmbot.service`
  (`Restart`, `UMask=0077`, `NoNewPrivileges`, `MemoryMax`, `TasksMax`, graceful
  `SIGTERM` stop). Build with `make vmbot`.
* `max_vms` (default 2) is only an application ceiling; keep the real resource policy
  on the daemon. Foreign VMs count against the backend, not against `max_vms`.

## Tests and verification boundaries

`make check` builds and tests everything with no root, KVM, network or Telegram token.
The offline suite uses a fake Bot API (`httptest`), `vmlibtest` and a **fake ttyd**
(the test binary re-executed with ttyd's flags: unix-socket HTTP server, upgrade echo
and a `setsid` child standing in for ssh). It proves allowlist, serialization, state
persistence/locking, stale callbacks, rotation, revocation closing an open upgraded
connection and killing out-of-group children, Host/Origin/token/query rejection, the
ssh argv/injection boundary and token-free errors.

Not proven offline, and to be checked on the host before relying on it:

1. `ttyd --version` is 1.7.x and accepts `--interface <unix socket>`,
   `--writable`, `--check-origin`, `--max-clients` as used (start fails loudly if not).
2. A real browser loads the page through the gateway: the CSP, WebSocket path
   (`<link>ws`) and `/token` fetch behave; a second tab beyond `max_clients` is refused.
3. `/terminal` -> shell in a real guest; `/delete` drops the open browser session and
   leaves no `ttyd`/`ssh` process (`pgrep`), and the old link returns 404.
4. Real Telegram delivery (preview off, no forwarding) and long polling.
5. `systemctl --user` start/stop/restart with the unit; VM survives a restart.
