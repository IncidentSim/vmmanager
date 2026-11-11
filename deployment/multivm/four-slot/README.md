# Four-slot proposal — not installed or activated

The existing two-slot examples and legacy shared slice remain unchanged. This
proposal is a **capacity change**, not permission to consume additional host
resources. Four `small` VMs each reserve 1 guest vCPU, 0.5 CPU of execution time,
500 MiB guest RAM and 64 MiB VMM overhead. Admission is 4 VMs / 4 virtual CPUs /
2 CPU-time units / 2000 MiB guest RAM. Including overhead requires 2256 MiB;
the proposed separate slice allows 2560 MiB and 200% CPU. The old shared
2048 MiB / 100% slice cannot provide this envelope. Do not merely raise max_vms.

The separate slice leaves the legacy slice unchanged, but adds capacity alongside
it. Review physical host RAM/CPU, existing/retained workloads, storage and other
services before approving it. Alternatively design and review a single aggregate
host envelope; do not silently combine or raise existing budgets.

## Required deployment steps (operator/parent only)

1. Integrate and test the exact reviewed revision. Build vmapi, fc-vm-helper and
   fc-vm-helper-client together: the helper wire protocol is now exactly
   `start-slot N CPU_MILLI\n` (e.g. `start-slot 0 500\n`). Old/new clients and
   helpers must not be mixed. Back up existing binaries/config/unit drop-ins.
2. Inventory all existing VM reservations and running unit identities. Pause new
   admission and settle in-flight helper calls; do not restart/kill running VMs,
   move their cgroups, remove retained disks or reset runtime CPUQuota drop-ins.
   Wait for safe, receipt-proven cleanup of affected slots before moving them to
   the separate slice. Profile edits never shrink existing reservations.
3. Review/approve capacity. If approved, install `firecracker-vms.slice` and
   `resources.conf` in `firecracker-vm@.service.d/`; retain the existing jail,
   storage-mount assertions, network dependencies and 768M per-unit memory cap.
   Replace @CLIENT_GROUP@ in the main templates. Install the updated helper
   template with `ReadWritePaths=/run/firecracker-vms` and network parser.
4. Ensure `/run/firecracker-vms` is root:root 0755 via the existing tmpfiles rule.
   Helper lock files `.helper-slot-N.lock` are created root:root 0600. They must
   never be group-writable, symlinks or unlinked/replaced while helpers run.
   The client-writable `.backend.lock` is a different lock; do not reuse it.
   `ProtectSystem=strict` requires the explicit helper ReadWritePaths exception.
5. Choose a collision-free GUEST_PREFIX, install the proposed pool file root:root
   0644, and run read-only network checks for slots 0..3 before enabling them.
   Install the matching backend config only after confirming its paths, actual
   image disk bounds, approved budgets, default profile and 500m root ceiling.
   Missing MAX_SLOT_CPU_MILLI remains compatible and defaults to 2000m; the
   four-slot proposal deliberately pins it to 500m.
6. Validate rendered units, reload systemd, replace the matched binaries and
   resume admission only after all prerequisites pass. Parent also updates its
   API/owner quota configuration (4 total / 2 per user); not part of this worker.
7. Run real Firecracker E2E: create four VMs from a configured bootable image;
   verify effective reservation and guest vCPU/RAM, per-unit
   `CPUQuotaPerSecUSec=500ms`, underlying cgroup `cpu.max` ratio, and parent slice
   200% / 2560M. Confirm a fifth admission fails and retained/stopped VMs still
   reserve quota until proven-safe deletion. Check each helper transaction's
   inactive check → runtime set-property → quota readback → start sequence.
   Verify invalid quota, busy slot and same-slot concurrent requests cannot
   mutate/start a running VMM. Clean up only test-owned IDs with safe Delete.

Local fake tests are not evidence that systemd accepted a quota or that this host
has spare capacity. No deployment commands in this proposal have been executed.
