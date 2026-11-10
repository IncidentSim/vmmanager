# Reference deployment: real multi-VM verification

Date: 2026-11-10. Firecracker 1.18.0, Linux x86_64 with KVM, Ubuntu 24.04 guest.
This is actual host execution, separate from root/KVM-free `make check`.

## Deployment exercised

- Separate multi-VM units: `firecracker-vm@N`, `firecracker-vm-network@N`,
  socket-activated `fc-vm-helper`. Legacy builder and its HTTPS manager remained active.
- Unprivileged `vmd` in a persistent user service. Combined mode: mTLS facade on
  port 19450 and independently authenticated backend on loopback port 19451.
- Runtime parent root:root 0755, precreated lock root:client-group 0660,
  systemd-created slot directories root:client-group 0750. Root-written receipts
  are readable by the client group, not writable by it.
- Operator pool `10.213.N.0/30`, two slots, separate TAPs. Network script ran on
  the host, while Firecracker and its static launcher ran in the filesystem jail.
- Work and image catalog on the existing bounded 20-GiB ext4 mount. No disk
  repartitioning or formatting, and no changes to legacy services/network.
- Real base is 3 GiB logical size, so this deployment uses a 4096-MiB disk policy
  rather than the illustrative 2048-MiB profile. Guest RAM remains 512 MiB and
  one vCPU per VM; two guests admitted.

## End-to-end result: PASS

The operator-run `scripts/integration/multi-vm.sh` completed successfully:

1. Start a real guest from `ubuntu-base` and authenticate over SSH through ProxyJump.
2. Write and sync a marker using an external SSH command (no playbook interpreter
   or application provisioning built into the library).
3. Gracefully stop, verify the original invocation's clean exit receipt, commit
   a new ext4 image and reject overwriting that image.
4. Delete the source workspace, then start two independent VMs from the new image.
5. Authenticate to both through ProxyJump; verify the persisted marker and distinct IPs.
6. Remove the marker and create a breakage file in one VM; the other retains its
   original data and has no breakage file.
7. Verify the base image SHA256 is unchanged. Cleanup deletes test VMs.

Base SHA256 before/after:
`f8a0439fe2c7d9a2ec3aa048e33d146be2faabb07c0e3329ee1fb151c4ea7597`.

## Additional real checks

- A client-side `vmlib.Service` over the remote backend creates a guest, while a
  remote facade client creates a second guest concurrently. Both reach authenticated
  SSH through ProxyJump. Third creation returns `ErrResourceExhausted`.
- Observed create-to-authenticated-SSH times (including disk copy, helper/systemd
  launch, guest boot and probing) were 5940 ms and 5875 ms in that run; these are
  observations, not latency guarantees or isolated kernel boot benchmarks.
- Both real VMM units belong to the same budget slice. Kernel cgroup values:
  parent `cpu.max = 100000 100000`, `memory.max = 2147483648`,
  `memory.swap.max = 0`; each VMM `memory.max = 805306368` (768 MiB).
  During the sampled two-guest run, parent memory usage was about 287 MB.
  Limits were inspected in the live kernel; destructive OOM tests and CPU
  saturation/load benchmarks were not performed.
- Real mTLS listener rejects missing client certificates, client certificates
  from the backend's different CA, and client-side hostname mismatch.
- Restarting only `vmd` leaves a real guest alive and reachable through SSH.
  Reconciliation reports its disk retained until exit is proved. Subsequent
  Delete requests identity-checked graceful shutdown, verifies exit and frees it.
- All test guest processes/workspaces were removed. Temporary bastion test
  authorization was removed and the original authorized-keys content restored;
  idle test TAP/NAT units were stopped. `vmd` and the helper socket remain active.

## Fixes discovered by real deployment

The daemon's restrictive `UMask=0077` caused `MkdirAll(..., 0770)` to create the
shared `work/vms` parent as 0700. The jailed VMM has group access but no DAC override,
so configuring the block device failed before guest start. Initialization now
explicitly restores 0770 on this parent; a regression test covers an existing 0700
parent. The failed VMM was stopped externally before releasing its workspace.

The integration address check must parse the top-level `SSHInfo.Host`, not grep
all `"Host"` lines: nested bastion metadata also contains a host. Its offline fixture
now includes nested bastion JSON.

## Verification boundaries

- ProxyJump used the actual host SSH server and SSH forwarding, but the client
  also ran on that host using a loopback bastion connection and a restricted
  temporary test key. This proves the jump path, not reachability from a particular
  laptop, mobile network or external firewall.
- The base's MAC-derived guest address was exercised. Default route/DNS remain
  explicit guest configuration metadata for external automation; no real Ansible
  playbook or PostgreSQL installation was run in this verification.
- Installed mount/cgroup guards were inspected. The shared storage was not
  deliberately unmounted, filled or reformatted during this run; earlier storage
  capacity evidence and offline fail-closed tests remain separate.
- Shared preinstalled demo SSH credentials and trusted-operator isolation remain
  limitations. No per-VM key injection, public multi-tenant authorization or live/RAM
  snapshots are claimed.
