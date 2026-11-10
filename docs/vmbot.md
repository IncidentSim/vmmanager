# vmbot: Telegram single-VM bot

Decisions: [ADR 0002](adr/0002-telegram-single-vm-bot.md). `vmbot` is an ordinary
mTLS client of the `vmd` facade (`remote.DialFacade`); it does not change daemon
policy and does not touch the legacy builder or the multi-VM deployment.

Source only: nothing here is installed, started or proven live (see
[Verification boundaries](#verification-boundaries)).

## UX (Russian, private chat only)

| Command / button | Effect |
|---|---|
| `/start`, `/help` | help and the persistent keyboard (Создать, Статус, Терминал, Удалить) |
| `/create` | create the one VM (refused if any VM exists); on success the status and then the secret terminal link are sent automatically |
| `/status` | state of the VM, with inline buttons (Статус, Терминал, Удалить) |
| `/terminal` | the secret terminal link (previews off, forwarding protected) |
| `/delete` | revoke the terminal, then delete the VM |

The inline *Удалить* button carries the VM generation: pressed on an old message
after the VM was replaced it answers "Устарело" and deletes nothing. Typing
`/delete` is explicit and acts on the current VM. Only the configured Telegram
user ID in a private chat with itself is served; any other sender, group or
callback is dropped without a reply.

## Exactly one occupied VM

* *Occupied* = a VM in any state, including stopped, failed and retained.
* Create is refused if the durable state holds a VM **or** the shared facade lists
  any VM (foreign VMs are never adopted, stopped or deleted) **or** listing fails.
* Duplicate Create calls are serialized by one lock; a second concurrent call gets
  "already occupied".
* A failed or unconfirmed delete keeps the slot; run `/delete` again. Only a
  successful `vmlib` Delete (a settle-and-block barrier) releases the slot; a `Get`
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

### Bounds

One `ttyd`, `max_clients` WebSockets (gateway 503 + `ttyd --max-clients`), at most 64
open gateway connections, header/read/write/idle timeouts, `MemoryMax`/`TasksMax`.

## State and recovery

`state_dir` (0700): `state.json` (0600, atomic temp+fsync+rename), `lock` (exclusive
`flock` for the process lifetime), `known_hosts/`, `ttyd-<vm>.sock`.

| Situation | Behaviour |
|---|---|
| crash/restart | VM kept; startup reconcile with `Get`; running VM keeps the same link and `ttyd` is restarted |
| crash during Create | identity+token were saved first; `Get` adopts the VM (even failed/retained/preparing); `NotFound` is resolved only by the Delete barrier, else the slot stays `creating`; an unprovable ambiguous Prepare stays retained until the operator verifies the backend and restarts `vmd` |
| crash during Delete | the `deleting` phase is persisted before the revoke, so the old link never returns; Delete is resumed |
| cannot persist state | Delete changes nothing (no revoke, no remote call) and says so |
| Create/Delete RPC timeout | occupancy kept; resolved by `Get`; never a second VM |
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
* Keep the VM slot policy on the daemon: size `max_vms` knowing this bot refuses to
  create when any VM exists.

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
