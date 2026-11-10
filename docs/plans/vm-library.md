# Implementation plan: VM library (see ADR 0001)

Each stage is one atomic commit with tests written first for handwritten
behavior. `make check` needs no root and no KVM.

## Module layout

```
proto/vmlib/backend/v1/backend.proto      HostBackend service
proto/vmlib/facade/v1/facade.proto        VMService facade
vmlib/gen/...                             generated Go (never hand edited)
vmlib/                                    public domain, Backend iface, Service facade
vmlib/vmlibtest/                          fake narrow backend for tests/consumers
vmlib/local/                              local host backend (linux)
vmlib/remote/                             mTLS helpers, backend+facade gRPC servers/clients
cmd/vmd/                                  app: backend-only | facade-only | combined
cmd/vm-launcher/, cmd/vmctl/              per-slot launcher (source only); facade CLI client
examples/pgprovision/                     PostgreSQL provisioning as an application over vmlib
scripts/gen-proto.sh                      pinned generation + `--check`
scripts/integration/multi-vm.sh           operator-run real verification (not run by make check)
deployment/multivm/                       proposed unit/helper/network sources + install notes
docs/                                     architecture, ssh, migration, verification
```

## Stages

1. **Docs** — ADR 0001 and this plan.
2. **Proto + pinned generation** — `.proto` files, `scripts/gen-proto.sh`
   (pinned buf, protoc-gen-go, protoc-gen-go-grpc), generated code, check mode.
3. **`vmlib` core** — domain types, errors, ID/profile validation, `Backend`
   interface, `Service` state machine; fake backend; external-package tests:
   create/start, SSH info, idempotent stop/delete, commit, concurrent ops,
   admission caps, unique leases, cancellation, retained disks, no-publish
   without receipt, no overwrite.
4. **mTLS + remote backend** — ephemeral-PKI tests over real loopback gRPC: no
   cert, wrong CA, wrong hostname, valid; backend server/client; ID/resource
   rejection; error mapping.
5. **Facade gRPC** — server over any `Service`; client implementing the same
   facade interface; composition tests: local, remote-backend, combined.
6. **Local backend** — resource pool (slot/TAP/IP/MAC), bounded admission,
   workspace copy, receipt reading, restart reconciliation, fail-closed; unit
   tests with injected launcher/VMM-control seams (not real VMs).
7. **`cmd/vmd`** — flags/config, signals owned here, modes, default raw backend off.
8. **Legacy compatibility** — existing CLI and HTTPS build API tests remain; a
   PostgreSQL provisioning example over `vmlib` (application layer).
9. **Deployment sources** — proposed templates/helper, operator integration
   script, Makefile entrypoints, docs (architecture, SSH examples,
   migration/rollback, verification limits).

## Verification matrix

| Claim | Evidence | Not covered |
|---|---|---|
| Orchestration, races, caps | fake-backend `go test -race` | real VM behavior |
| mTLS enforcement | loopback gRPC, ephemeral PKI | production PKI handling |
| Path/ID rejection | unit + RPC tests | host kernel behavior |
| Local backend logic | seam-injected tests | real Firecracker/TAP/launcher |
| Real multi-VM flow | `scripts/integration/multi-vm.sh` (operator) | needs approval, root deploy |

Deployment-dependent checks stay open until the operator installs the proposed
units and runs the integration script.

## Post-review fixes (second pass)

Module renamed to `github.com/petruekhin/vibe-firecracker`; Service race fixes and
facade contract; local backend ownership/retention/launch-intent/ABA/durability/
admission/mount guard; socket-activated helper, split networking, operator-chosen
pool, collision checks, real `systemd-analyze verify`; guest network metadata and
readiness semantics; integration-script sequencing. See the ADR amendments.
