# Deployment resources

Use [the Go installer](../docs/install.md) for the current multi-VM product.
`make build` builds the executables; `make install` installs binaries, rendered
units, tmpfiles rules and examples without activating services or networking.
`DESTDIR` permits a root-free staged install. Deployment resources are embedded
in `vmhost`, so release archives need no source tree.

- [Installation and prerequisites](../docs/install.md).
- [Multi-VM deployment](multivm/README.md).
- [Network policy and refresh](multivm/NETWORK.md).
- [API/owner migration](../docs/control-api.md).
- [Disposable CI/KVM E2E](../docs/ci.md).

The old single-VM builder Bash installer, fixed-network deployment and image
preparation wrappers are retired; see [ADR 0009](../docs/adr/0009-go-tooling-install-ci.md).
Existing hosts are not changed by source cleanup. Follow the exclusive-owner
handoff before changing an existing deployment; never replace a live helper
protocol or restart a VMM just to update files.
