# SSH access and external automation

The project returns connection metadata; it does not run Ansible, create your
bastion account, or route traffic from outside the host. Guests live on
host-private addresses (`PREFIX.N.2` (your `pool.conf` prefix, e.g. 10.213), unique per VM); you reach them with standard
OpenSSH `ProxyJump` through a bastion **you** operate on the host.

## What `SSHInfo` contains

```
Host, Port, User          guest private IP, 22, ssh user from server policy
AuthMode                  image-preinstalled | provision-public-key | client-key-reference
KeyReference              a label YOU resolve locally (never a server path, never key bytes)
PublicKeyProvisioned      true only if the server injected your public key into this VM's disk copy
HostKeyPolicy/Fingerprint accept-new-per-vm (default) or pinned when a fingerprint is known
Bastion                   host/port/user from server policy (optional)
Guest                     address (CIDR), gateway, DNS and who configures it (see below)
Ready                     set by WaitSSHReady; means what the prober proved (see below)
```

No CA key, server private key or shared SSH private key is ever returned.

## Commands (examples; run by you, not by this project)

```sh
# vmctl talks to the HTTP control API. The bearer is read from a private (0600)
# file or VMCTL_TOKEN, never from an argument; vmctl waits for the operation.
VMCTL="vmctl -url https://api.example.com:8443 -token-file $HOME/.config/vmctl/token"

$VMCTL create -image ubuntu-base -name demo1   # prints the API-assigned vm_id
$VMCTL ssh-info VM_ID                     # JSON
$VMCTL ssh-info -format ssh -known-hosts ~/.ssh/fc-demo1.known_hosts \
        -key ~/.ssh/id_ed25519 VM_ID    # args for ssh, shell-quoted

# Plain ssh through the bastion (ProxyJump) - equivalent to the printed args:
ssh -J jump@bastion.example.com -i ~/.ssh/id_ed25519 \
    -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=~/.ssh/fc-demo1.known_hosts \
    root@10.213.0.2
```

### Ansible inventory

```sh
{ echo "[guests]"; $VMCTL ssh-info -format ansible -key ~/.ssh/id_ed25519 demo1; } > inv.ini
ansible-playbook -i inv.ini your-playbook.yml     # YOUR playbook, YOUR ansible
```

which prints e.g.

```
demo1 ansible_host=10.213.0.2 ansible_port=22 ansible_user=root \
      ansible_ssh_private_key_file=/home/me/.ssh/id_ed25519 \
      ansible_ssh_common_args='-o ProxyJump=jump@bastion.example.com'
```

Then `stop`, `commit demo1 golden`, create two VMs from `golden`, run another
playbook, `delete` them. `scripts/integration/multi-vm.sh` is this exact flow
(operator-run).

## Key handling and trust limits

* **Preinstalled shared key (`image-preinstalled`)**: the demo base logs in with a
  key baked into the image. Anyone holding that key can log into *every* VM derived
  from that base or any committed descendant. Treat it as a demo credential.
* **Public-key provisioning (`provision-public-key`)** injects *your public key*
  into a per-VM disk copy, never the base. The shipped local backend does **not**
  implement injection (it needs privileged mounting) and rejects such requests; a
  future backend can. Base login compatibility is never broken by forced key
  regeneration.
* `KeyReference` is a client-side label. The server cannot know your key paths and
  does not pretend to.
* Guest host keys are not known before first boot. Use a per-VM
  `UserKnownHostsFile` with `accept-new`; do not disable host-key checking globally.
* The bastion account (a user on the host that may `ProxyJump` to your guest pool (e.g. `10.213.0.0/16`))
  is yours to create and restrict (e.g. `PermitOpen`); this project does not.

## Guest network contract (supported base images)

`SSHInfo.Guest` tells external provisioning how the guest is configured:

* `Configuration = "mac-derived"`: the base derives its address from the MAC
  (06:00:A:B:N:02 -> A.B.N.2, as the Firecracker CI Ubuntu rootfs does) and applies a
  /30. **It still has no default route and an empty resolver.** Your provisioning
  must install both before using the network, from `Guest.Gateway` and `Guest.DNS`
  (the PostgreSQL example does this before `apt-get`). Without `Guest.DNS` the server
  policy supplies none (`guest_dns`) and provisioning that needs the network will fail.
* `Configuration = "external"`: the base does not self-configure; you must set the
  address, route and DNS yourself over a console or image mechanism. The library does
  not do this for you.
* Arbitrary bases are not assumed to support MAC-derived addressing. Check yours.

## What "ready" means

`WaitSSHReady` returns when the configured prober succeeds. The default prober only
checks that TCP port 22 accepts a connection **from the process running the
library**; it does not authenticate, and a remote client cannot reach a
host-private guest at all. For a meaningful signal use the authenticated prober
through your bastion:

```go
p := vmlib.SSHCommandProber{KeyFile: "/home/me/.ssh/id_ed25519", KnownHostsFile: "/home/me/.ssh/fc.known_hosts"}
svc, _ := vmlib.NewService(backend, vmlib.WithSSHProber(p.Probe))
info, err := svc.WaitSSHReady(ctx, vm.ID) // ssh -J ... true succeeded
```

## Reconciling a failed Create

At the `vmlib` level Create takes a caller ID (generated when empty). If the call
fails or the response is lost, `Get(id)` tells you whether a VM exists and
`Delete(id)` removes it. Over the HTTP API the VM ID is server-assigned and the
`Idempotency-Key` makes a retried create return the same operation.
