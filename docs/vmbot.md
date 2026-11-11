# vmbot (historical)

> **Historical.** The standalone single-tenant Telegram bot that this guide
> described (its own Telegram client, JSON config, controller and DBOS
> create/delete/terminal/reconcile workflows in `internal/vmbot`) has been
> removed from the code base. It is not the running application.

Current runtime:

* `cmd/vmbot` is an HTTP-only Telegram frontend (`internal/tgbot`) that talks to
  the control API through `controlclient`; see [bot-api-client.md](bot-api-client.md).
* `cmd/vmapi` serves the control API and drives VMs through the per-VM owner
  workflows in `internal/vmbot` (`owner.go`, `control.go`, `terminal.go`); see
  [control-api.md](control-api.md), [architecture.md](architecture.md) and the
  [RFC](rfc-vm-owner.md).

Kept as historical record (decisions and evidence, not operating instructions):
[ADR 0002](adr/0002-telegram-single-vm-bot.md),
[ADR 0003](adr/0003-vmbot-multi-vm.md),
[ADR 0004](adr/0004-vmbot-dbos-orchestration.md),
[DBOS verification](vmbot-dbos-verification.md) and
[live verification](vmbot-verification-live.md). The removed guide text, UX
tables, config reference and unit hardening notes remain in git history before
the cleanup described in [vm-owner-cleanup-verification.md](vm-owner-cleanup-verification.md).
What survives of the old workflow is only a retirement stub and the old state
fields needed to migrate stores; see that document.
