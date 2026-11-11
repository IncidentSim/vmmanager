# vibe-firecracker

Self-hosted Firecracker microVM management: HTTP API, Go client/library and an
optional Telegram frontend. The host runs Linux, systemd and KVM. This is not a
single-file hypervisor distribution: Firecracker, a compatible guest kernel and
rootfs, storage and host networking must also be prepared.

## Components

- `vmapi`: authenticated VM/image operations, ownership and resource quotas,
  durable DBOS/SQLite recovery, SSH access and terminal integration. The local
  Firecracker backend runs in-process; no separate VM RPC daemon is needed.
- `vmbot`: HTTP-only Telegram client; it does not own the VM lifecycle.
- `vmctl` / `controlclient`: CLI and Go HTTP client.
- `vmlib`: public Go lifecycle library for create, SSH metadata, graceful stop,
  cold disk-image commit, delete and independent derivatives.
- `vmhost`: Go installer, uninstaller, host preflight/setup and unit verification.
- `vm-network`, `vm-helper`, `vm-helper-client`, `vm-launcher`: host networking,
  narrow privileged slot launch, and trusted per-invocation exit receipts.

Commit is an offline disk publication after confirmed shutdown, **not** a live
RAM snapshot. API ownership does not make the underlying trusted-operator host
backend a hardened multi-tenant boundary.

## Build and checks

Use the Go version declared in `go.mod` or newer, Make, and Linux. Root-free
checks never start a guest or change host services/firewall rules.

```sh
make build
make check
make control-smoke
make help
```

`make check` runs Go vet, formatting and race tests, builds the product, verifies
an exact staged install/uninstall round trip and renders/checks systemd units.
`control-smoke` drives the real API entry point with an in-memory backend; it is
not a real Firecracker E2E.

## Installation

See [installation and host prerequisites](docs/install.md) for the complete
contract, files/modes, configurable paths, preflight, storage and activation.

```sh
make build
make install DESTDIR="$PWD/stage" CLIENT_GROUP="$(id -gn)"  # no root, test layout
make uninstall DESTDIR="$PWD/stage" CLIENT_GROUP="$(id -gn)"
# On a prepared host, as administrator:
sudo make install CLIENT_GROUP=vmusers
```

Installation places binaries, systemd units, tmpfiles rules and example configs;
it does not enable services, change networking, overwrite active configuration
or prepare/format a volume. Host setup and helper activation are explicit steps.
Never run competing backend owners against the same VM storage; follow the
[migration guide](docs/control-api.md) for an existing deployment.

Tagged [GoReleaser](.goreleaser.yaml) releases contain static host executables and
portable CLI archives with checksums. The installer embeds its deployment
resources, so extracted host archives need no source checkout:

```sh
sudo ./vmhost install --group vmusers
```

GoReleaser is a build/release tool, not a runtime dependency. Validate and build a
local unpublished snapshot using `make release-check`.

## E2E and CI

[CI](docs/ci.md) separates root-free checks from real KVM tests on a disposable
Linux host. `make e2e-kvm` requires provisioned KVM/systemd/artifacts and fails if
prerequisites are absent. [Live E2E](docs/e2e-live.md) can exercise an explicitly
configured service with an opt-in principal and tracked disposable resources:

```sh
VMAPI_E2E=1 make e2e-live   # requires the documented VMAPI_E2E_* settings
```

Do not point disposable-host provisioning at a production machine. CI workflows
use no production server or credentials. A passing unit test or generated unit
check is not evidence of real guest boot or external SSH reachability.

## Guides

- [Architecture](docs/architecture.md) and [ADRs](docs/adr/).
- [Control API](docs/control-api.md), [OpenAPI](docs/control-api-openapi.yaml).
- [Username/JWT/Swagger](docs/username-jwt-swagger.md), [bot client](docs/bot-api-client.md).
- [Common image registry](docs/common-image-registry.md), [SSH automation](docs/ssh-automation.md).
- [Guest networking](deployment/multivm/NETWORK.md).
- [Recorded live verification](docs/verification-live.md): historical evidence
  for the named revision/deployment, not automatic acceptance of current changes.

## Legacy consumers

The root Go package and `cmd/remote-client` / `cmd/service-launcher` remain tested
compatibility consumers of the original single-VM image builder (`make legacy`).
They are not installed or packaged as the current product. Their old host
installer, fixed-network deployment, Bash/Python verification and demo wrappers
have been retired; see [ADR 0009](docs/adr/0009-go-tooling-install-ci.md).
The only remaining `.sh` file is an embedded **guest boot** template, not a host
installer or test harness. Host orchestration and tests are Go-native.
