# ADR 0008: SSH-idle expiry of VMs

**Status:** Approved policy; implemented in the VM management service (`vmapi`).
The web trainer is not involved and is not changed.

## Policy

- Applies to **all** VMs of the service, whether created through the bot, the web
  front end or the HTTP API, including VMs that predate the feature.
- **Any established SSH connection keeps a VM alive**, including an idle terminal,
  an SFTP session or an SSH forwarding connection. Keyboard inactivity is not
  measured.
- When the VM has had **no established SSH connection for more than 600 seconds**
  (configurable, `idle_timeout_seconds`), the service deletes it through the
  existing lifecycle owner. A new connection resets the window.
- `ttl_seconds`/`expires_at` stays an independent upper bound.
- Pending Create, a VM that is not `active`, and any VM with an unfinished owner
  command are protected. Provisioning and verification over SSH are protected by
  being connections; the window starts at the **first successful observation**,
  which doubles as the boot grace period.

## Architecture

There is no second lifecycle controller. The monitor is a read-only observer that
may only **admit an ordinary `delete` owner command** (operation ID `op-idle-*`,
owner-authored like the TTL expiry delete; the key contains a space so no caller
can collide with or replay it). The per-VM owner workflow runs it serialized with
every other command through the unchanged `begin -> effect -> project` path, so
disk, slots, VMM stop, credential and Nexterm cleanup are unchanged. No durable
call sequence of the owner changed, so no `dbos.Patch` is needed and old owner
histories replay untouched.

### Observation

Every `idle_poll` (30 s) the monitor visits each VM in phase `active` (or a failed
`deleting` retry) whose lifecycle state is `running`, one at a time, each probe
bounded by a timeout.

The probe is a fixed read-only command run over SSH from the service host to the
**internal** guest address (`SSHInfo.InternalTarget`, never the public endpoint)
using the VM's dedicated managed terminal credential lease and strict host trust
(`StrictHostKeyChecking=yes` against the operator's `personal_ssh`
`shutdown_known_hosts_file`; it is never auto-populated):

    printf '%s\n' "$SSH_CONNECTION"; ss -H -n -t state established '( sport = :22 )'

The guest tuple in `SSH_CONNECTION` identifies the probe's own connection. The
count is the number of established TCP/22 sockets **except that exact tuple**
(IPv4, IPv6 and IPv4-mapped IPv6 are normalized). It is not "total minus one": if
the monitor's own tuple is absent or duplicated, or any line is malformed, the
observation is an error.

Without credentials, known-hosts trust or a working guest `ss`, the observation
is **unsupported/failed and never leads to deletion** (fail closed).

### State machine (in memory, per VM)

| Observation | Effect |
|---|---|
| connections > 0 | window cleared |
| probe error / ambiguity / VM not running / not eligible / busy | window cleared, **never delete** |
| zero connections | window starts at the first such observation |
| zero for > `idle_timeout` | final probe, then admit `delete` |

Before the owner executes the admitted delete (`beginOp`, the existing checkpointed
step) the monitor probes once more; anything other than a clean zero cancels the
command (`failed`, code `conflict`) and the window restarts. A failed ordinary
delete follows the existing semantics (the VM stays `deleting`, resources are
retained); the next idle window admits a new `op-idle-*-N` for the same VM.

### Restart

The window is **not durable**. After a `vmapi` restart every VM needs a fresh
600 s of continuous zero observations. This is deliberately conservative and
avoids a new store or schema. Legacy VMs without TTL are included.

## Limits (accepted)

- **Observation race:** a connection that arrives after the final check but before
  the VMM is stopped is not protected. No atomic guarantee is claimed.
- A guest root user can influence `ss` and `SSH_CONNECTION`. Accepted for this PoC.
- TCP keepalive/conntrack is not used as activity.
- Guests need `ss` (iproute2) and a POSIX login shell for the managed user.
