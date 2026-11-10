# ADR 0003: Several VMs in the same private vmbot

Status: Accepted
Date: 2026-11-10
Baseline: commit `98f90d9`; amends [ADR 0002](0002-telegram-single-vm-bot.md)

## Context

The operator approved owning several VMs from the same private chat. `vmlib` and
`vmd` already run VMs concurrently, so this is an application change inside
`internal/vmbot`: no new daemon or supervisor, no change to `vmlib`, to daemon
policy or to host limits. A VM is already live under the v1 state file
(`bot-c5d5cd1e8a7894eb`); the upgrade must adopt it, not recreate it.

Unchanged from ADR 0002: single allow-listed user in a private chat, no TTL, no
summaries, no audit feature, identity persisted before `Create`, `NotFound` never
proves absence (only the `Delete` barrier does), `deleting` persisted before the
terminal revoke and the remote `Delete`, no fake success when persistence fails.

## Decisions

### Occupancy and capacity

* The application owns the VMs in its durable state. It never adopts, lists for
  adoption or deletes foreign VMs. The "refuse when `List` shows any VM" rule of
  ADR 0002 is dropped: foreign VMs already consume backend capacity, and the
  backend's own admission (`ErrResourceExhausted`, reference capacity 2) stays the
  authority. The bot reports that outcome in Russian and keeps no record of it.
* `max_vms` (config, default 2, range 1..8) is an application ceiling on top of
  that. A record in any phase and any remote state (creating, stopped, retained,
  uncertain, deleting) counts. A failed or retained delete does **not** free its
  occupancy; only a successful `Delete` removes the record.

### Concurrency

* The global operation lock is replaced by one lock per VM ID. Different VMs
  create, reconcile, delete and open terminals independently; a slow Create of A
  never blocks B.
* The only shared critical section is admission, done inside one atomic state
  update: name uniqueness, `max_vms` and generation allocation are checked and the
  new record (`creating`) is written in the same persist, so two concurrent
  creates cannot both take the last slot or the same name. The new VM's lock is
  taken before its record becomes visible.
* Status/list reconciles every VM in parallel with a bounded wait; a busy VM is
  shown from its durable record, never hidden. Startup recovery reconciles every
  record independently: one unreachable or ambiguous VM neither hides nor deletes
  another.

### State schema v2 and migration

```
{"version":2,"generation":N,"offset":N,
 "vms":[{"id","token","phase","generation","label"?}, ...]}   // ascending generation
```

* Generations are unique and monotonic and never reused; the VM list is kept in
  ascending generation order, which is the deterministic display order.
* `label` is the unique, immutable, optional display name (`[a-z]([a-z0-9-]{0,18}[a-z0-9])?`,
  must not start with `bot-`). It is never used in a path, socket, command or
  argv; IDs are. Selection is by exact label or exact VM ID.
* Validation (duplicate IDs/tokens/generations/labels, bad IDs, bad tokens, bad
  phases, generation above the high-water mark, negative offset, unknown fields,
  trailing data) happens **before** anything is mutated; failures and unknown
  future versions fail closed and the process refuses to start.
* **v1 -> v2** runs in `OpenStore` under the lifetime lock: the v1 file is fully
  validated; the single record is carried over field for field (ID, token, phase,
  generation) together with `generation` and `offset`; it receives the label
  `vm-<generation>`. Order: (1) write the exact v1 bytes to
  `state.json.v1.bak[.N]` (0600, `O_EXCL`, fsync - an existing backup is never
  overwritten; an identical one from an earlier retry is reused, a different one
  gets a new numeric suffix), (2) atomically replace `state.json` (temp, fsync,
  rename, directory fsync). A crash between the two leaves valid v1 and the next
  start retries. Backup failure aborts the migration (fail closed).
* The old callback `d:<generation>` keeps working because generations are
  preserved: it selects the migrated original and can never hit a replacement
  (which has a larger generation).

**Rollback limits (honest).** An older binary refuses v2. Restoring
`state.json.v1.bak` is only safe **before the first v2 write**. After v2 runs it
loses the advanced Telegram offset (updates may be replayed, breaking
at-most-once for create/delete), forgets every VM created after migration (they
stay on the backend, unowned and still consuming capacity), forgets generation
history (reuse of numbers), and v1 can hold only one VM. The bot logs the backup
path at migration. Rollback after additional VMs exist requires deleting them
first and restoring the backup manually with the bot stopped.

### Telegram UX (Russian)

`/create [name]`, `/list`, `/status [sel]`, `/terminal [sel]`, `/delete [sel]`.
With exactly one VM the selector may be omitted; with several the bot shows the
list with per-VM buttons and does nothing else: never "latest", never "all".
Callbacks are `s:<gen>`, `t:<gen>`, `d:<gen>` (<= 22 bytes, limit 64): a
generation is unique forever, so a button names exactly one VM; a missing
generation answers "Устарело" and cannot retarget. Legacy `s`/`t` act only when
unambiguous, otherwise show the list. The list is split over several messages
(by entry, under the 4096-character limit) so no VM is truncated. A creation
success sends that VM's secret link as before (previews off, protected content).

### Terminal manager

One session per VM: a private unix socket and `known_hosts` per VM, the same
fixed `ssh` argv, no shell. Capability routing looks the token up across sessions
without early exit. `Revoke(A)` terminates only A's ttyd, its ssh descendants and
its tracked WebSockets; B keeps its connections and URL. `Close` terminates all.
Slow start/termination and proxying never run under the manager lock; a start
in flight is cancelled by `Revoke`/`Close` and cannot outlive them.

Each VM has one *slot* that exists from `Start` until **all** cleanup (processes,
WebSockets, socket and `known_hosts` files) has finished, whichever goroutine does
it (the starter, a `Revoke`, `Close` or a restart after the backend died). The slot
keeps counting against the session bound and keeps its capability token reserved
while pending, active **and terminating**, so one token can never route to two VMs.
`Revoke` and `Close` return only after every affected slot is fully cleaned, even
when another goroutine owns the termination. A link stops routing the moment
termination begins. A `Start` for a VM whose previous session is still shutting
down is refused (retryable) instead of publishing a replacement over half-cleaned
files, so the old cleanup can never unlink a new session's socket or
`known_hosts`; unrelated VMs are never blocked. Bounds:
`max_vms` sessions, `max_clients` per VM, and `max_vms * max_clients <= 64` (the
existing gateway connection limit) is enforced at config load.

## Consequences

* A bug in admission could exceed `max_vms`; covered by concurrency tests.
* Memory held per VM is one small lock; locks are not garbage-collected
  (bounded by VMs created during the process lifetime).
* Rollback after multi-VM use is manual (above).
* Not proven offline: real backend capacity error text, real ttyd with two
  simultaneous sessions, live migration of the running v1 state. These are
  deployment checks, outside this change.
