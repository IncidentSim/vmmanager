# Host resource boundaries

These are administrator-managed deployment settings, not client API parameters.
The application never starts/stops a host unit or changes these limits.

## Aggregate VMM CPU and RAM

All `firecracker-builder@*.service` instances use systemd's automatic parent
`system-firecracker\x2dbuilder.slice`. Apply `resource-budget.conf` to that slice,
not independently to every instance. Alternatively the operator can persist:

```sh
sudo systemctl set-property 'system-firecracker\x2dbuilder.slice' \
  CPUQuota=100% MemoryMax=2G MemorySwapMax=0
```

On approval-gated hosts wrap privileged commands with the approved root-command
wrapper. CPUQuota100% means a total of one CPU's execution time, not pinning to a
particular physical core. Guest RAM and vCPU threads belong to the VMM cgroups;
their overhead also counts. The separately running user API/provisioning process
has its own memory limit and is not included in this VMM-only budget. This does
not add multiple-VM provisioning to the current single-VM application.

Kernel readback should show `cpu.max` as `100000 100000`, `memory.max` as
`2147483648` and `memory.swap.max` as `0` on the parent slice.

## Bounded disk space

Use a dedicated real 20-GiB filesystem for BOTH `/var/lib/firecracker-builder/work`
and the remote manager's job/artifact directory. Bind mount separate `work` and
`jobs` subdirectories from that filesystem at the existing paths, preserving the
client and jail path contract. This is a hard shared capacity ceiling; ext4
metadata consumes part of it. CPU cgroups do not cap disk capacity.

Mount by filesystem UUID using persistent systemd mount units. The VMM unit
must have `RequiresMountsFor=/var/lib/firecracker-builder/work` and
`AssertPathIsMountPoint=/var/lib/firecracker-builder/work` in a `[Unit]` drop-in.
The user manager must have an `ExecStartPre=/usr/bin/mountpoint --quiet JOB_DIR`
check so it cannot silently write to the root filesystem when storage is absent.
Stop clients and VMM before migration; preserve previous directories and confirm
there are no active workspace lock holders. Do not format an existing partition.

The fixed Ubuntu base and server executables stay outside this VM/artifact budget.
An operator explicitly exporting a local CLI result outside these paths is also
outside the managed artifact budget; remote callers cannot select that path.

Verify both paths have the same `st_dev`, different from `/`, and inspect
`findmnt` and `df`. With builders stopped, `posix_fallocate` requesting 21 GiB
in that filesystem must return ENOSPC; immediately remove the disposable probe.
Never use `truncate` alone as a quota test: a sparse file's logical size may
exceed actual storage capacity without allocating any blocks.

## Running host layout

The deployed server uses `/dev/sda3`, exactly 20 GiB, mounted at
`/var/lib/firecracker-builder/storage` with work/jobs bind mounts. An additional
40 GiB were added online to the pre-existing LVM root filesystem. The original
boot partition and partition starting sectors were preserved. The original
partition-table backup is stored outside this repository. No repartitioning or
formatting is performed by the application or ordinary deployment installer.

VM cancellation deadlines and completed-artifact retention remain separate
policies. There is no persistent, force-enforced VM TTL or automatic artifact
expiry yet; a full storage volume rejects further allocations until the operator
removes unneeded completed artifacts safely.
