# ADR 0007: Remove the standalone gRPC VM plane

Status: accepted (source stage). Supersedes the gRPC transports of ADR 0001 and
the mTLS facade connection of ADR 0005.

## Context

`vmd` served the host backend and a `vmlib.Service` facade over two mTLS gRPC
listeners; `vmapi` dialed the facade. On the single host this added two network
hops, generated code, a PKI and a second long-running daemon for no isolation the
HTTP API did not already provide (JWT username binding, per-user ownership,
quotas, idempotency).

## Decision

* The HTTP control API is the only user-facing plane and the only DBOS owner.
* `vmapi` instantiates `vmlib.Service` over `vmlib/local.Backend` in-process,
  from a private `backend` policy defined once in `internal/localcfg` (same
  fields and validation as the old `vmd` `backend.local`). Test-only trusted
  UID/ancestor seams stay out of JSON. No fake backend exists in the deployed
  binary; tests inject one through an opener seam.
* Removed: `cmd/vmd`, `proto/`, `vmlib/gen`, `vmlib/remote` (and its PKI test
  helper, kept as `internal/pkitest`), the protobuf generator tooling and checks.
  Replaced by `make no-grpc-check`. `vmctl` is now an HTTP client of the control
  API (bearer from a private file or environment, never argv).
* Kept untouched: the remote builder HTTPS/mTLS manager, Firecracker Unix-socket
  forwarding, the host helper, systemd/network/resource/receipt/publication
  safety, and the portable public SDK (`vmlib`, `vmlibtest`, `controlclient`),
  which still cross-builds.
* A stale `facade` section in the API config is rejected, not ignored.

## Consequences

* The backend's exclusive lifetime flocks forbid running it beside the old
  `vmd`: stop the bot, the API and `vmd` before the first direct start. Close
  releases locks only; no VM is stopped or deleted; all metadata, identities,
  leases, reservations, markers and receipts carry over. State (JSON + SQLite +
  WAL) is backed up together while stopped and is never blindly restored after
  new writes.
* gRPC/protobuf may remain transitive through third-party dependencies; only
  application use is removed.
* Isolation is a proof of concept: `vmapi` holds only the public JWT key, but it
  runs as the same OS user as the helper client and SSH key, so any same-UID
  process can read those private files. Unprivileged user namespaces were
  observed unavailable on the reference host, so user-unit `ProtectHome=` /
  `BindPaths=` style settings are not enforced and are not claimed. No new
  root/user isolation is invented.
* Old `vmd` config, binary and PKI are kept privately by the operator for a
  deliberate rollback, which needs a current-state compatibility review.
