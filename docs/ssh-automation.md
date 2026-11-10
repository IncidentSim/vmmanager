# SSH access and external automation

The project returns connection metadata; it does not run Ansible, create your
bastion account, or route traffic from outside the host. Guests live on
host-private addresses (`172.17.N.2`, unique per VM); you reach them with standard
OpenSSH `ProxyJump` through a bastion **you** operate on the host.

## What `SSHInfo` contains

```
Host, Port, User          guest private IP, 22, ssh user from server policy
AuthMode                  image-preinstalled | provision-public-key | client-key-reference
KeyReference              a label YOU resolve locally (never a server path, never key bytes)
PublicKeyProvisioned      true only if the server injected your public key into this VM's disk copy
HostKeyPolicy/Fingerprint accept-new-per-vm (default) or pinned when a fingerprint is known
Bastion                   host/port/user from server policy (optional)
Ready                     set by WaitSSHReady
```

No CA key, server private key or shared SSH private key is ever returned.

## Commands (examples; run by you, not by this project)

```sh
VMCTL="vmctl -addr vmd.example.com:19450 -server-name vmd.example.com \
        -ca ca.crt -cert client.crt -key client.key"

$VMCTL create -image ubuntu-base -profile small -id demo1 -key-ref laptop-ed25519
$VMCTL ssh-info demo1                     # JSON
$VMCTL ssh-info -format ssh -known-hosts ~/.ssh/fc-demo1.known_hosts \
        -key ~/.ssh/id_ed25519 demo1      # args for ssh, shell-quoted

# Plain ssh through the bastion (ProxyJump) - equivalent to the printed args:
ssh -J jump@bastion.example.com -i ~/.ssh/id_ed25519 \
    -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=~/.ssh/fc-demo1.known_hosts \
    root@172.17.0.2
```

### Ansible inventory

```sh
{ echo "[guests]"; $VMCTL ssh-info -format ansible -key ~/.ssh/id_ed25519 demo1; } > inv.ini
ansible-playbook -i inv.ini your-playbook.yml     # YOUR playbook, YOUR ansible
```

which prints e.g.

```
demo1 ansible_host=172.17.0.2 ansible_port=22 ansible_user=root \
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
* The bastion account (a user on the host that may `ProxyJump` to `172.17.0.0/16`)
  is yours to create and restrict (e.g. `PermitOpen`); this project does not.
