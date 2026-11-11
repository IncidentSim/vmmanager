# ADR 0009: Go-native host tooling, installer and CI

Status: accepted (implementation in progress on `refactor/go-tooling-ci`)

## Context

The active product is `vmapi` (embedded local Firecracker backend, HTTP API),
the HTTP `vmbot` client, the root helper/launcher/network units for VM slots,
public SSH publication, personal SSH policy, Nexterm access and SSH-idle expiry.
Its host tooling had grown around it as Bash and Python: a 335-line firewall
script run by a systemd unit, an installer for a *different* (legacy single-VM
builder) service, Python unit/firewall tests that mock `ip`/`iptables`, and
shell checks wired into `make check`. There was no CI, no product installer,
and the default `make build` produced the legacy `image-builder`.

## Decisions

### Cleanup boundaries

Removed (with replacement):

| Removed | Replacement |
|---|---|
| `deployment/multivm/fc-vm-network.sh` | `cmd/vm-network` + `internal/vmnet` (same ownership/collision/cleanup rules) |
| `deployment/multivm/test_network.py`, `test_deploy.py` | Go tests in `internal/vmnet`, `internal/hostinstall` |
| `scripts/verify-units.sh` | `internal/hostinstall` unit-rendering tests + `systemd-analyze` test |
| `scripts/no-grpc-check.sh`, `scripts/test_no_grpc.py` | `internal/repocheck` Go test |
| `scripts/consumer-smoke.sh` | `internal/repocheck` Go test (external-module consumer, cross-builds) |
| `scripts/integration/multi-vm.sh`, `test_multi_vm.py` | live Go E2E in `e2e/` (one-off operator flow; documented sequential acceptance flow kept in docs) |
| `deployment/install.sh`, `uninstall.sh`, `prepare-ubuntu.sh`, `network.sh`, `generate-mtls.sh`, `test_mtls.py`, `firecracker-builder*.service`, `resource-budget.conf` | **Legacy single-VM builder host deployment: capability removed.** Not replaced; see below |
| `scripts/run-image.sh`, `scripts/demo-lifecycle.sh`, `verify-image.py`, `test_verify_image.py`, `test_project_layout.py` | Legacy builder helpers: capability removed. Layout test replaced by Make/installer Go tests |
| `deployment/multivm/vmd.example.json` and other stale proposals | Only if verified unreferenced; see commit inventory |

Retained deliberately:

* All Go packages, public libraries (`vmlib`, `controlclient`, `vmlib/vmlibtest`)
  and commands. The legacy builder *Go sources* (root `main` package,
  `cmd/service-launcher`, `cmd/remote-client`) are not imported by the product
  and are kept buildable with `make legacy` and still tested, but they are no
  longer built by default and no installer or unit deploys them. Whether to
  delete them outright is left to a separate decision.
* ADRs, architecture/API docs, verification reports that record real evidence.
* `internal/sshaccess/supported/fcnet-setup.sh`: this is **guest** boot content
  embedded in provisioned images (it runs inside the microVM, not on the host).
  It is the one justified shell exception; porting it would require shipping a
  guest Go binary for no benefit.

Legacy capability explicitly **not preserved**: installing and operating the
single-VM `firecracker-builder@main.service`, its `tap-fcpg` network unit, the
builder mTLS certificate generator, the image-builder runtime helpers
(`run-image.sh`, `demo-lifecycle.sh`, `verify-image.py`) and the polkit revoke
logic. Hosts that already run that service keep working; they are simply no
longer installed/managed from this repository. The multi-VM units no longer
require the legacy service to exist.

### Go tooling instead of shell recipes

* `cmd/vm-network`: root-run slot TAP/NAT/firewall tool executed by
  `firecracker-vm-network@.service`. Pure Go; external commands (`ip`,
  `iptables-nft*`, `conntrack`) go through a runner interface so ownership,
  collision, hook-precedence and cleanup rules are unit-tested with a model.
* `cmd/vmhost`: unprivileged-capable installer, uninstaller, preflight and unit
  renderer, backed by `internal/hostinstall`.
* Makefile is thin orchestration only (build, test, check, install, e2e).

### Install contract

* `make install [PREFIX=/usr/local] [DESTDIR=]` stages and installs only files.
  It never starts, enables, reloads or reconfigures anything, never touches
  firewall, storage, `pool.conf`, keys, images or state.
* Layout under PREFIX: `bin/{vmapi,vmbot,vmctl,vmhost}`,
  `libexec/{fc-vm-launcher,fc-vm-network,fc-vm-helper,fc-vm-helper-client}`,
  `lib/systemd/user/{vmapi,vmbot}.service`, `share/doc/vm-firecracker/examples`.
  Root-managed system units go to `SYSCONFDIR/systemd/system`, tmpfiles to
  `SYSCONFDIR/tmpfiles.d`. Units are rendered against the actual installed
  paths and the client group; no site-specific values are embedded.
* An ownership manifest (paths, modes, SHA-256) is written under
  `PREFIX/share/vm-firecracker/`; `make uninstall` removes only manifest-listed
  files that still exist, never user data, config, keys, images or state.
* Destination safety: every path must stay inside DESTDIR; symlinked path
  components or symlinked targets are refused; writes are atomic.
* Deliberate host steps are separate commands: `vmhost preflight` (read-only,
  actionable KVM/systemd/tools/subnet/storage report), `vmhost setup` (creates
  the empty jail/work/images directories under an *existing* storage mount; never
  formats) and `vmhost enable` (daemon-reload + enable the helper socket). Both
  require explicit `--apply`, otherwise they print the plan.

### CI contract

* Root-free job: `go test -race`, `go vet`, build, Go tooling tests, staged
  install round trip, API executable smoke (in-memory backend over loopback; named
  as a smoke, never as E2E).
* Real E2E job: a disposable Linux runner with `/dev/kvm`. The job installs the
  product with `vmhost` from pinned, SHA-256-verified Firecracker/kernel/rootfs
  artifacts, runs the existing `e2e/` suite against a vmapi it starts itself,
  and tracks/cleans only fixtures it created. The dedicated target fails (never
  skips) when KVM, tools or artifacts are missing. `make e2e-live` against a
  deployed server keeps its explicit opt-in refusal.
* Hosted-runner KVM availability is an assumption that must be checked by the
  job itself and is documented; the workflow gates any self-hosted fallback
  against untrusted fork PRs.

## Consequences

Unverified here by construction: real KVM boot, real systemd activation, real
firewall mutation and remote CI execution. These are run only by CI/operators.
