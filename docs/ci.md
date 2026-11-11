# Continuous integration

Workflows live in `.github/workflows/`. Go comes from `go.mod` through
`actions/setup-go`. Checkouts do not persist credentials. PR jobs have only
`contents: read`, cancel superseded runs, and use no production credentials.
Workflow and release contracts are also checked by `internal/repocheck`.

| Workflow | Trigger | Evidence |
|---|---|---|
| `ci.yml` / `go` | PR, push to `main` | pinned actionlint; `make check` (vet, formatting, race tests, product build, staged installer round trip, required `systemd-analyze`); executable API smoke; legacy tools build |
| `ci.yml` / `release-snapshot` | same | GoReleaser v2.20.1 OSS `check`, real snapshot builds, archive extraction, extracted `vmhost stage-check` and unit verification without a source tree |
| `e2e-kvm.yml` | PR, push to `main`, manual | real Firecracker guest lifecycle and SSH on a disposable KVM host |
| `release.yml` | `v*` tag push only | `make check`, then GoReleaser publishes GitHub Release archives and SHA-256 checksums; only this workflow has `contents: write` |

The executable API smoke uses the **in-memory backend** on loopback. It is not a
Firecracker E2E. Snapshot jobs never publish; release runs are not cancelled.

## Real E2E

The KVM job provisions a **clean, disposable** Linux x86_64 host:

1. Require `/dev/kvm`; missing hardware is a failure, never a successful skip.
2. Install host networking/filesystem tools, `squashfs-tools` and `fakeroot`;
   explicitly enable IPv4 forwarding and mount a newly formatted 12 GiB loopback
   ext4 volume at `/var/lib/firecracker-builder/storage`. This deliberately
   destructive fixture setup belongs only on a disposable runner.
3. Build the product and `e2e-ci`. Fetch the three artifacts in
   `e2e/ci/artifacts.json` over HTTPS with bounded downloads and SHA-256 checking.
4. `fakeroot bin/e2e-ci prepare-image`: extract the verified Ubuntu squashfs,
   convert it to ext4 without mounting, preserve guest ownership, and add the
   udev-settle prerequisite to the upstream fcnet unit. On a disposable copy,
   exercise the **actual product SSH and guest-network provisioners** before
   accepting the image. Credentials and slot networking never enter the base.
5. Install via the built `vmhost install` (the same installer as `make install`);
   this avoids relying on Go being on sudo's PATH. Generate fresh TLS, token,
   SSH master key, collision-free pool and API configuration under a **new**
   private directory, then install root-owned `pool.conf`.
6. Explicit `vmhost setup --apply`, `vmhost enable --apply`, read-only preflight
   and base-image installation; existing image files are never overwritten.
7. `make e2e-kvm` starts its own vmapi on loopback, waits for TLS health and runs
   `go test -json ./e2e`. Inherited `VMAPI_E2E*` and TLS environment settings are
   discarded. Core lifecycle, derivative-image, credential hygiene, supplied-key
   login, finite TTL, profile and public API checks **must actually pass**:
   missing or skipped required tests fail the harness even if `go test` exits 0.
8. Delete leftover VMs only for the generated test principal, stop vmapi, always
   collect journals and test JSON, stop fixture units, uninstall product files
   and unmount/remove the loopback fixture.

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
Upstream downloads are not included in product release archives.

To update, download the candidate from the upstream project over HTTPS, calculate
its SHA-256, update the URL/hash/member/bound together, then run `e2e-ci fetch`
and `fakeroot e2e-ci prepare-image` before running the disposable KVM job. A
changed/missing artifact or incompatible guest provisioning format must fail,
not trigger an unverified fallback. Bucket object names alone are not immutable;
the pinned digest is authoritative. The fetch/preparation/offline provisioner
checks do **not** prove bootability: only a successful real KVM job does.

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

Run `make release-check GORELEASER=/path/to/goreleaser` with GoReleaser v2 OSS.
It writes real archives and `checksums.txt` into ignored `dist/`, publishing
nothing. PR CI also runs the extracted installer, so an archive that merely
compiles but cannot install is not accepted.
