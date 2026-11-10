# ADR 0006: Telegram username access, RS256 user tokens and Swagger

Status: accepted for the PoC (gRPC removal is a separate, later stage).

## Decision

* Operators allow people by Telegram **username**; the immutable resource owner
  stays the numeric Telegram ID. A bot cannot resolve arbitrary usernames, so
  identity comes only from Telegram's own sender (`message.from.username` + ID)
  in a **private chat whose chat ID equals the sender ID**. Usernames compare
  case-insensitively, an optional leading `@` is accepted in config, the ASCII
  grammar (5-32 chars, letter first, `[A-Za-z0-9_]`) is enforced and
  case-insensitive duplicates are rejected. Display names, message text and
  callback data never authorize anything. A missing/unlisted username fails
  closed before any API work.
* The first allowed contact **pins** the username slot to that numeric ID in a
  durable file (`username-bindings.json`, 0600, flock + atomic replace + fsync).
  Concurrent first claims have one winner. A recycled or renamed handle can never
  move the slot to another ID; pins last until the operator edits the file
  (stop the service first). Removing access (config change + restart) never
  deletes VMs; ownership fences honour *bindings and the configured allowlist*.
* The **bot holds the RS256 private key** and signs user tokens; the **API holds
  only the public key** and verifies (`github.com/golang-jwt/jwt/v5`, RS256 only,
  required `iss`, `aud`, `exp`, `nbf`, `iat`, positive canonical decimal `sub`,
  token length <= 4096, lifetime <= 7 days, RSA >= 2048 bits, 3072 recommended).
  Default lifetime is 28800 s (8 h); `/token` again renews.
* Token claims: `sub` = numeric ID; optional signed `tg_username` bootstraps the
  matching configured slot. Unsigned/unverified claims are never used. A token
  whose ID does not match the pinned slot is rejected (403).
* The bot's own API calls use short per-user signed tokens too; there is no
  service credential or delegation (`X-Acting-User-ID` is refused for user
  tokens). Static credentials keep working when configured.
* `expose_api_on_terminal` serves `/v1/*`, `/healthz`, `/docs`, `/docs/assets/*`
  and `/openapi.yaml` on the existing HTTPS terminal gateway, in-process on fixed
  routes (no forward URL), keeping exact Host/Origin/capability/WebSocket checks.
  Browser requests to API routes with a foreign `Origin` get 403; no CORS headers.
* Swagger UI is self-hosted (`swagger-ui-dist` 5.34.1, Apache-2.0, vendored with
  checksums in `internal/controlapi/swaggerui/PROVENANCE.md`); no CDN.

## Consequences / limits

* A leaked token is valid until expiry (<= configured TTL); there is no
  revocation list. Rotate the key pair to invalidate everything.
* The bot's pin store and the API's pin store are separate files; the API's is
  authoritative. They agree because the API only sees tokens the bot signed after
  its own first-claim. If the bot's file is lost, the API still refuses a
  recycled ID.
* Telegram usernames are mutable; the pin protects against a *recycled* handle
  only after the first legitimate contact. Before first contact, whoever owns the
  handle at that time wins the slot. Operators who need certainty should list
  the numeric ID (`allowed_user_ids`) or have the intended person contact the bot
  first.
