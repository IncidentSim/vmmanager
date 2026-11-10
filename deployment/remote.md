# Remote manager deployment (mTLS)

The HTTPS manager runs as the same trusted local user as the existing SDK client.
It connects to the already-running Firecracker Unix socket; it does not start or
stop host systemd units. Do not publish the raw Firecracker API.

The trust model is one trusted operator: any client certificate signed by the
private client CA is authorized to build, read and cancel jobs. This is not a
multi-tenant sandbox or per-user artifact isolation. Keep port 19443 separate
from existing HTTPS/proxy services.

## Certificate material

On the Linux server with OpenSSL 3:

```sh
mkdir -m 700 "$HOME/firecracker-remote"
bash deployment/generate-mtls.sh "$HOME/firecracker-remote/pki" server.example.com
```

The output directory must not already exist. The CA is valid for ten years;
server and client certificates are valid for 90 days. The server identity has
serverAuth only; the client identity has clientAuth only. Private files are
created with mode 0600 inside a 0700 directory. The hostname must match the
endpoint; certificate verification must not be disabled.

Copy **only** `ca.crt`, `client.crt` and `client.key` to the laptop over a trusted
channel. Never include `ca.key` or `server.key` in a client bundle or source Git
repository. Anyone possessing the client key can operate the manager. CA-key
compromise requires replacing the trust root and all certificates. Removing a
client file locally does not revoke copies of it: this initial setup has no CRL
or per-certificate revocation endpoint; replace the CA to revoke issued clients.

## User service

Build the root application with `go build -o "$HOME/firecracker-remote/image-builder" .`.
Create private `jobs` and `tmp` subdirectories of `$HOME/firecracker-remote`.

Use the supplied `firecracker-remote.service` as a **user** systemd unit, replacing
`@HOME@` and `@PROJECT@` with absolute directories. Install it under
`~/.config/systemd/user/`, then run:

```sh
systemctl --user daemon-reload
systemctl --user enable --now firecracker-remote.service
```

The template listens on TCP 19443 with mandatory mutual TLS. It does not change
firewall rules or other services. Confirm user lingering if the service must
survive logout/reboot; enabling lingering may require administrator approval.

## Resource boundaries

The manager's `MemoryMax=512M` is for its process and local provisioning child,
**not** for the separately running root VMM. Guest RAM/vCPU limits belong to the
VMM's cgroup. For multiple VMM units, aggregate memory/CPU budgets must be on a
shared parent slice. Disk-space quotas require a filesystem quota or bounded
filesystem, not a cgroup I/O limit. Do not describe a 20-GiB disk quota as applied
merely because the manager checks free space.

Only one VM/build is supported at a time. Job cancellation must wait for the
existing builder's receipt-based cleanup; never delete a disk still in use.
An execution timeout is not a persistent, force-enforced VM TTL: hard expiry and
recovery across manager restarts require a separate supervisor/reconciliation
mechanism. Completed artifact retention should be configured independently of
VM lifetime.
