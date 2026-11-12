# SSH through a separate forwarding server

This is an operator recipe for the existing Linux/systemd deployment, not a new
backend feature or a clean-host installer. The application's root-pool address
remains the backend's literal address. `public_ssh.advertised_host` can name the
separate frontend server; the operator owns its DNS, DNAT, tunnel and return route.

## Do not confuse advertised addresses with local service routing

Keep the Nexterm URL consistent with the currently served certificate. Removing
the old hostname from SAN requires updating the API's `nexterm.url` as well.
Where the backend cannot hairpin through the frontend, use a narrowly reviewed
host-local hostname mapping for local HTTPS. The Nexterm container needs its own
mapping to the backend address covered by its existing restricted SSH hairpin
rules, not the host's loopback mapping. Preserve the hostname in URLs so TLS
validation remains enabled. These local mappings do not alter public DNS.

## Firewall precedence is part of the deployment contract

A slot network refresh inserts its `FCVM*F` hooks near the start of `FORWARD`.
Those chains terminate in `DROP`. A separate tunnel allow chain or Nexterm
hairpin chain below the slot hook will never see the relevant packets. Merely
creating the additional chains at tunnel startup is insufficient.

For the existing deployment, `IP_EXTRA_VM` already permits only the four guest
SSH endpoints arriving over the tunnel, with exact guest/port, connection mark
and original-destination matches. Its creation/removal belongs to WireGuard
PostUp/PostDown. `NXHAIRPIN` owns the independent container-to-guest path. Do not
replace either with a generic FORWARD accept rule or flush unrelated chains.

Install this root-owned helper as `/usr/local/libexec/ip-extra-vm-refresh`
(mode 0755) on that deployment. It moves only the exact existing tunnel hook,
atomically, and does not create or broaden the tunnel chain:

```python
#!/usr/bin/python3 -I
import fcntl
import os
import subprocess

assert os.geteuid() == 0
fd = os.open('/run/ip-extra-vm-refresh.lock',
             os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
with os.fdopen(fd, 'w') as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    def run(args, **kwargs):
        return subprocess.run(args, capture_output=True, text=True, **kwargs)

    # WireGuard owns chain setup. A missing chain is not permission to allow all.
    result = run(['/usr/sbin/iptables-nft', '-w', '-S', 'IP_EXTRA_VM'])
    if result.returncode:
        raise SystemExit(0)
    hook = '-m comment --comment ip-extra-vm -j IP_EXTRA_VM'
    result = run(['/usr/sbin/iptables-nft', '-w', '-S', 'FORWARD'], check=True)
    existing = [line for line in result.stdout.splitlines()
                if line == '-A FORWARD ' + hook]
    rules = ('*filter\n'
             + ''.join('-D FORWARD ' + hook + '\n' for _ in existing)
             + '-I FORWARD 1 ' + hook + '\nCOMMIT\n')
    run(['/usr/sbin/iptables-nft-restore', '-w', '--noflush'],
        input=rules, check=True)
```

Install this drop-in after the existing Nexterm `ExecStartPost` drop-in as
`/etc/systemd/system/firecracker-vm-network@.service.d/80-extra-ip-ssh.conf`:

```ini
[Service]
ExecStartPost=/usr/local/libexec/ip-extra-vm-refresh
ExecReload=/usr/local/libexec/nexterm-hairpin apply
ExecReload=/usr/local/libexec/ip-extra-vm-refresh
```

The template's original `ExecReload` must still refresh the slot first. Keep the
existing Nexterm `ExecStartPost` so startup restores both paths too. Reload the
systemd manager and run the refresh helper; do not restart a live slot network.
This recipe requires the pre-existing root-owned helpers and exact chain names.
It intentionally does not change the frontend server, pool, VM disks or keys.

## Acceptance and rollback

1. Record a live test VM ID and the current VMM invocation.
2. Run two non-disruptive `systemctl reload firecracker-vm-network@N.service`
   cycles. Verify the tunnel hook precedes slot DROP hooks, with the Nexterm
   hairpin hooks before the slot hooks for container traffic.
3. Require an independent TCP connection to the frontend's actual published SSH
   port, not a request originating on the backend or its own local DNS mapping.
4. Run a harmless marker through the deployed Nexterm engine using the exact
   ordinary account, entry and SSH identity. Do not infer this from HTTP 200.
5. Create a fresh disposable VM and repeat both tests after guest SSH is ready;
   a successful create operation can precede sshd startup. Delete only tracked
   diagnostic VMs and verify deletion, preserving other tenants' VMs.
6. A saved drop-in establishes persistence configuration, not a completed reboot
   test. Explicitly distinguish repeated reload/new-VM checks from a full reboot.

To undo, remove only this drop-in and helper, then daemon-reload. That returns
precedence maintenance to its previous behavior; it does not undo existing
WireGuard/Nexterm rules. Keep a private backup of replaced deployment files.
