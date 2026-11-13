# Continuous integration

Workflows live in `.github/workflows/`. Go comes from `go.mod` through
`actions/setup-go`. Checkouts do not persist credentials. PR jobs have only
`contents: read`, cancel superseded runs, and use no production credentials.
Workflow and release contracts are also checked by `internal/repocheck`.

| Workflow | Trigger | Evidence |
|---|---|---|
| `ci.yml` / `go` | PR, any branch push | pinned actionlint; `make check` (vet, formatting, race tests, product build, staged installer round trip, required `systemd-analyze`); executable API smoke; legacy tools build |
| `ci.yml` / `release-snapshot` | same | GoReleaser v2.20.1 OSS `check`, real snapshot builds, archive extraction, extracted `vmhost stage-check` and unit verification without a source tree |
| `e2e-kvm.yml` | PR, any branch push, manual | real Firecracker guest lifecycle and SSH on a disposable KVM host |
| `release.yml` | `v*` tag push, manual retry of an existing tag from `main` | `make check`; then (release job only) apt-installs `fakeroot squashfs-tools e2fsprogs`, runs `make bundle` unprivileged, and GoReleaser publishes the archives, `checksums.txt`, `vmmanager-example.tar.gz` and `vmmanager-example.tar.gz.sha256`; only this workflow has `contents: write` |

The executable API smoke uses the **in-memory backend** on loopback. It is not a
Firecracker E2E. Snapshot jobs never publish; release runs are not cancelled.

The root-free Go job creates a private `TMPDIR` below the runner user's home.
Credential fixtures use `t.TempDir()` and intentionally exercise production
ancestry checks: a private leaf below shared writable `/tmp` is not trusted.
For equivalent local checks, supply a temporary directory below a trusted,
non-group/world-writable home; never relax the credential validators. Assembly
tests supply their own pool files rather than reading the host's active pool.
The legacy workflow recovery test force-kills an isolated test executor; normal
cooperative shutdown can complete the workflow and is not crash evidence.

## Real E2E

The KVM job provisions a **clean, disposable** Linux x86_64 host:

1. Require `/dev/kvm`; missing hardware is a failure, never a successful skip.
2. Install host networking/filesystem tools, `squashfs-tools` and `fakeroot`;
   explicitly enable IPv4 forwarding and mount a newly formatted 12 GiB loopback
   ext4 volume at `/var/lib/firecracker-builder/storage`. This deliberately
   destructive fixture setup belongs only on a disposable runner.
3. Run `VMAPI_E2E_DISPOSABLE=1 make e2e`. Make builds the product and invokes
   **one** `go test ./e2e`; `TestMain` and ordinary `_test.go` helpers own the
   fixture lifecycle, with no additional command or runtime package.
4. TestMain requires passwordless sudo, a fresh host (no existing pool or
   installed product manifest), KVM/TUN, a mounted storage volume, and tools.
   It downloads `internal/fixture/artifacts.json` pins over HTTPS with bounds and
   SHA-256 verification. Image preparation runs unsquashfs and mkfs.ext4 under
   fakeroot with a shared ownership database; the existing offline SSH/network
   provisioners validate a disposable copy. The upstream fcnet unit gets its
   examined udev-settle prerequisite; no credentials enter the base image.
5. Before boot, read the guest ed25519 host public key from the derived ext4
   using read-only debugfs and create exact per-slot known_hosts entries. This
   offline, hash-verified trust source is supplied to the existing strict SSH
   checks; no network TOFU or relaxed host checking is used.
6. Generate fresh private TLS/token/master-key/config files, choose a
   collision-free pool, then use the built **shared vmhost installer** through
   explicit `sudo -n`: install, setup, enable, preflight. Only a newly created
   base image is copied into the fixture images directory (exclusive create).
7. Start the fixture vmapi on loopback, scrub inherited `VMAPI_E2E*`/TLS
   settings and run the existing real lifecycle tests directly via `m.Run()`.
   Required tests are recorded after their test cleanup; missing, skipped or
   failed required tests fail TestMain even if a test filter would exit zero.
   Optional checks still print their explicit skip reasons. Credential hygiene
   inspects both owner and terminal authorized_keys installed by Prepare, then
   verifies their removal from a committed derivative; it does not require a
   configured browser-terminal backend or claim browser-session acceptance.
8. Per-test ownership trackers clean admitted resources. TestMain also drains
   leftovers for the fresh API principal, stops its subprocess, removes its
   base image, stops fixture units, disables the socket, removes the pool and
   uninstalls through vmhost. Reverse-order rollback also runs on partial setup
   failures and reports cleanup failures. Workflow-level always-cleanup is a
   fallback for cancellation or forced test timeout; it collects journals,
   unmounts/removes its loopback fixture and archives the test output.

### Local disposable KVM host

Use a **fresh, throwaway** Linux x86_64 systemd host, never an existing VM
service host. Install the workflow's listed packages, grant KVM access, enable
IPv4 forwarding and mount an empty ext4 storage volume at
`/var/lib/firecracker-builder/storage` (at least 12 GiB). These host preparations
are deliberate operator actions, not part of `go test` or `make check`. With
passwordless sudo available to the test user, run:

```sh
VMAPI_E2E_DISPOSABLE=1 make e2e
```

Without the explicit opt-in, `make e2e` fails before building or preparing a
host. Ordinary `go test ./...` and `make check` remain root/KVM-free. To test an
already deployed service instead, use the separately gated `make e2e-live`;
it never invokes disposable preparation. Do not export the disposable opt-in
in a developer shell or combine it with the external-deployment settings.

The reused suite has separate optional checks for Nexterm, public SSH/egress,
physical CPU readback, a second principal, controlled slow-create TTL behavior,
quota saturation and API restart. This job does not claim those optional
acceptances: skip events are explicit in logs and the uploaded test JSON.
No service restart, VMM boot, firewall or mount operation is part of root-free
checks or local release packaging.

### Artifact provenance and updating pins

The manifest pins Firecracker **v1.12.1**, kernel **6.1.128**, and the Firecracker
project's **Ubuntu 24.04 CI squashfs** under its `firecracker-ci/v1.12/x86_64/`
S3 prefix. All three were downloaded from their declared URLs and their actual
SHA-256 values recorded. The Go fetcher was exercised against the full manifest.
The generated ext4 image is derived locally, not a pretend downloadable artifact.
Upstream downloads are not included in the product host/client archives; they
are included only in the separate release asset `vmmanager-example.tar.gz`
(see Releases).

To update, download the candidate from the upstream project over HTTPS, calculate
its SHA-256 and update URL/hash/member/bound together in
`internal/fixture/artifacts.json`, then run the disposable suite. A changed/missing
artifact, incompatible provisioning format or missing offline host key fails,
never falls back to unverified data. Bucket object names alone are not
immutable; the digest is authoritative. Download/image/unit tests do not prove
bootability: only a successful real KVM job does.

### Runner contract and fork safety

* Linux x86_64, systemd PID 1, root through sudo, `/dev/kvm`, `/dev/net/tun`,
  outbound HTTPS, at least 12 GiB free fixture storage and 4 GiB RAM.
* Default is GitHub-hosted `ubuntu-24.04`. Nested KVM availability depends on the
  runner/account. The workflow installs the documented KVM udev rule, but fails
  if the device is missing; no mock or successful skip substitutes for hardware.
* A self-hosted fallback is opt-in via repository variable `E2E_RUNS_ON`, a JSON
  runner specification, e.g. `["self-hosted","linux","x64","kvm"]`. It must be
  ephemeral and disposable, without production access or credentials. It runs
  repository PR code with root privileges: use only trusted same-repository
  branches. **When the override is configured, fork PRs skip this KVM job**;
  they still run root-free CI/snapshot jobs. With the default hosted runner,
  fork PRs may run KVM. Never use `pull_request_target` to run PR checkout code.
* Keep the KVM job required in branch protection if missing KVM must block
  merging; configuration and local unit tests cannot establish runner support.

## Releases

Push an intended `vX.Y.Z` tag to publish; no tag is created by these workflows.
`.goreleaser.yaml` builds static (`CGO_ENABLED=0`) Linux amd64/arm64 product
executables, including `vmhost` with embedded units, templates, tmpfiles and
examples. The extracted archive is the installer's build directory. Portable
`vmctl` clients are additionally built for Darwin amd64/arm64 and Windows amd64.
See [install.md](install.md#release-archives) for installation.

Each release also carries the **example bundle** `vmmanager-example.tar.gz`
(Ubuntu ext4 base image, Firecracker, kernel, configs and product binaries; see
[LOCAL-RUN.md](../LOCAL-RUN.md)) and `vmmanager-example.tar.gz.sha256`
(`sha256sum -c` format; the bundle is not listed in `checksums.txt`). The release
job builds it with `make bundle` from the hash-pinned upstream artifacts in
`internal/fixture/artifacts.json`: no secrets or state, an unprivileged image
assembly with fakeroot, scratch in the private home-owned `TMPDIR`, outside the
checkout so GoReleaser sees a clean tree. GoReleaser uploads it through
`release.extra_files` (globs from `VMMANAGER_EXAMPLE_DIR`). Branch CI and
`make check` never build it, download images or need root/KVM; local
`make bundle` needs network access and the same three packages.

Run `make release-check GORELEASER=/path/to/goreleaser` with GoReleaser v2 OSS.
It writes real archives and `checksums.txt` into ignored `dist/`, publishing
nothing. PR CI also runs the extracted installer, so an archive that merely
compiles but cannot install is not accepted.
