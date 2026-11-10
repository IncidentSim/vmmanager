# Telegram/web-terminal live verification

Reference host verification on 2026-11-10. This supplements the root/KVM-free
`make check`; it is not a claim that a mocked terminal proves real SSH.

## Exercised

- Ubuntu package ttyd 1.7.4 was installed. Its automatically enabled default
  service was stopped and disabled; the application owns its private instance.
- A disposable VM was created through the application Controller, the existing
  mTLS facade and the installed Firecracker host backend.
- The real ttyd listened on a Unix socket inside a private application-state
  directory, executing the fixed SSH argv with a server-side private key.
- The application HTTPS gateway served the actual ttyd page. A browser rendered
  Ubuntu 24.04.5 LTS, ran a marker command and reported guest hostname
  `ubuntu-fc-uvm`. The shell was the guest's, not the host's.
- A separate stdlib WebSocket probe exercised the actual ttyd upgrade and ran a
  guest marker command. Opening the same capability URL repeatedly returned200;
  a wrong token returned404, hostile Origin403, URL command arguments400.
- Stopping the disposable harness called Controller.Delete before gateway
  shutdown. An already-open WebSocket was actively closed; Delete completed,
  facade List was empty, and the test ttyd/SSH processes were gone.
- The prepared systemd user-unit sandbox was tested with a transient unit using
  ProtectHome=tmpfs and explicit read-only binds. Reading access to the token
  path was confirmed without printing its contents.
- A separately supplied bot token was verified with Telegram `getMe`; only
  allowlisted bot metadata was returned. The owner's private Telegram chat was
  verified with `getChat`, without consuming any queued updates.

## Boundaries

The browser and direct WebSocket clients ran on the server. Off-host browser
reachability is not established by those checks. At this stage real Telegram
command delivery, production service activation and restart recovery with the
final reviewed binaries still need verification.

The lifecycle adversarial tests cover interrupted creation, delete/restart
ordering and per-Service cancellation admission. They do not establish durable
cross-daemon cancellation tombstones. See ADR0001 and ADR0002 for retained-state
operator recovery and the single-use-ID contract.

No live capability URLs, bot credentials, private keys or filled runtime state
belong in this report. Deployment-specific credentials and configuration remain
outside the repository.
