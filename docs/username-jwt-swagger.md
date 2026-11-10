# Telegram usernames, RS256 tokens and Swagger (operator guide)

See [ADR 0006](adr/0006-telegram-usernames-rs256-tokens-swagger.md) for the
design. All names, IDs and hosts below are synthetic.

## Keys (outside Git)

Create a 3072-bit RSA pair outside the repository and never paste it into chat,
CLI arguments or logs:

```sh
umask 077
openssl genrsa -out jwt-private.pem 3072
openssl rsa -in jwt-private.pem -pubout -out jwt-public.pem
chmod 600 jwt-private.pem; chmod 644 jwt-public.pem
```

The private key goes only to the bot host config (`jwt.private_key_file`, owner
only, mode 0600, absolute path). The API gets `jwt.public_key_file`; it refuses a
private key there.

## Configuration

API (`deployment/vmapi/vmapi.jwt.example.json`): `allowed_usernames` (may carry
`@`, case-insensitive, no duplicates), optional legacy `allowed_user_ids`
(at least one identity policy is required), `jwt` {`public_key_file`, `issuer`,
`audience`}, `credentials` may be `[]` with a valid `jwt`, and
`expose_api_on_terminal: true` (requires `terminal.public_origin`; the same key is
also accepted inside `terminal`). The API root still listens on the configured
loopback `listen_addr`.

Bot (`deployment/vmbot/vmbot.jwt.example.json`): `allowed_usernames`, optional
`allowed_user_ids`, `jwt` {`private_key_file`, `issuer`, `audience`,
`ttl_seconds` (default 28800, maximum 604800)}. `api_token_file` may be omitted
in JWT mode and is then never read. Use a delivery directory separate from the API
state directory.

`issuer`/`audience` must match on both sides. The pin files are
`<api state_dir>/username-bindings.json` and `<delivery_dir>/username-bindings.json`.

## Using it

1. The allowed person writes `/token` to the bot in a private chat. The reply is a
   protected-content message with a code block and no link preview. It states the
   lifetime (8 h by default); send `/token` again to renew.
2. Open `https://<terminal public origin>/docs`, press **Authorize**, paste the
   token (no `Bearer ` prefix) and use *Try it out*; or
   `curl -H "Authorization: Bearer $TOKEN" https://<origin>/v1/vms`.
3. Tokens never need bot-only headers; `X-Acting-User-ID` is refused for them.

Changing the pin (operator action): stop the service, edit the entry in
`username-bindings.json` (keep mode 0600), start the service. Revoking access:
remove the username from the config and restart; VMs are kept.

## Verification

`make check` runs the unit/integration tests, including the real `vmapi` binary
smoke (`TestExecutableJWTAndComposedHTTPSGateway`: public-key-only JWT, HTTPS
composed gateway, Swagger/spec routes, pin persistence over restart; fake VM
backend and synthetic tokens). Live Telegram delivery, a real browser
*Try it out* session and real VMs are not covered by tests.
