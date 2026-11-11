# Offline personal SSH module

`internal/sshaccess` supplies `OfflineAccess` and `Credentials` seams. The local
backend and terminal manager use them when explicitly enabled. This is not evidence of a
successful real guest boot, SSH authentication, ttyd connection or live Commit.
No shared bootstrap identity or guest SSH connection is used by this module.

## Operator configuration and wiring

Personal access is off by default. Add this operator-owned object to the local
backend JSON only after preparing a persistent master key and compatible images:

```json
"personal_ssh": {
  "enabled": true,
  "store_dir": "/var/lib/vmapi/ssh-credentials",
  "master_key_file": "/var/lib/vmapi-secrets/ssh-master.key"
}
```

The paths above are examples, not private-key values. Startup never generates a
master key or substitutes a default secret. Provision exactly 32 cryptographically
random raw bytes as the service user (for example using `GenerateMasterKeyFile`),
mode 0600 or stricter, in a trusted non-symlinked parent not writable by others.
The master key must be outside the store. The store is service-owned mode 0700;
unsafe symlinks/ownership/permissions and malformed/missing master keys fail
startup. The service PATH must include the installed `debugfs` executable.
Keep the master key backed up separately from the encrypted store: losing either
prevents key retrieval and blocks managed-image publication. Do not rotate the
file in place without migrating all records. One service process owns the store.

The exported integration point is:

```go
func localcfg.BuildWithAccess(l *localcfg.JSONConfig, s localcfg.Seams) (*local.Backend, sshaccess.Credentials, error)
```

It returns the backend and its shared credential provider (nil when disabled).
`localcfg.Open` and `JSONConfig.Assemble` also configure the real disk hooks.
The reusable `vmlib/local` package depends only on its own `OfflineAccess`
interface (`Install`, `ScrubVM`, `Delete`), not on an internal credential type.
Pass the returned provider to `vmbot.ManagerConfig.Credentials` and the
owner-authorized API retrieval adapter. Never put that provider or private keys
in DBOS arguments, results, events, domain JSON, or logs. Only public access
intent hashes and the managed flag are persisted in workspace metadata.

Prepare copies the immutable base, makes only that workspace copy writable,
installs the owner and distinct terminal keys, syncs, then marks it prepared.
Failures discard the workspace copy and cannot boot. Repeated/restarted Prepare
preserves the VM identity and rejects changed access intent. Interrupted incomplete
preparation is retained and must be safely deleted before a retry.
Capabilities advertise provisioned-key access only with configured hooks; each
VM's `PublicKeyProvisioned` is derived from its own successful preparation.

Cold Commit requires the original invocation's clean exit receipt. It scrubs
only a separate publication copy using the trusted host installation receipt,
then publishes read-only with no overwrite. The stopped VM source and base are
never scrubbed. Failed scrub removes the temporary output and its marker, keeps
the source and credentials, and publishes nothing. A managed VM still requires
its configured scrubber after a server configuration change or restart.

Terminal uses the dedicated per-VM `Store.TerminalKeyFile` lease, held through
subprocess Wait and released on exit, revoke, close, or failed startup. Runtime
keys default to `StateDir/terminal-keys` (set `KeyRuntimeDir` to a private tmpfs
directory if needed). A missing personal credential never falls back to the
owner's key or a shared key. When personal access is disabled (nil provider),
legacy VMs continue using the configured `SSHKeyFile` unchanged.

Revoke and drain terminal processes before Delete. Backend Release removes
credentials only after confirmed durable workspace removal; failed deletion or
unproven exit retains them. The controller must additionally call
`Credentials.Delete(vmID)` only after a confirmed successful Lifecycle Delete,
to cover already-absent fast paths (e.g. failed Prepare left only credentials).
An error, retained result, or uncertain effect is never permission to delete keys.

## Required integration order

1. Provision a fresh random 32-byte server master key using
   `GenerateMasterKeyFile`, outside workflow/domain state. `OpenStore` uses a
   private service-owned directory. Keep one service owner for that directory;
   this package does not supply a cross-process store lifetime lock.
2. After obtaining exclusive ownership of a fresh **unmounted per-VM disk copy**,
   call `Provisioner.Install(ctx, vmID, disk, suppliedPublicKey)` before boot.
   Empty supplied key means stable generated Ed25519 owner credentials; supplied
   keys never get an owner private key. Both modes create an independent per-VM
   terminal identity. Replays preserve both identities; changing the intent fails.
3. Install must succeed before boot. On any provisioning error discard the working
   copy: debugfs edits are not transactional, and errors may follow earlier writes.
   Do not erase credentials on ambiguous Create rollback. Keep a stable VM ID and
   serial lifecycle owner; do not install different disks concurrently for one ID.
4. Owner-authorized private-key retrieval uses `OwnerPrivateKey`; generated-only,
   `Cache-Control: no-store`, never logs or workflow inputs/results/events.
5. Terminal creation leases a unique 0600 private-key file in a service-owned 0700
   runtime directory, ideally tmpfs. Ancestor symlinks are refused. Writable
   ancestors outside a service-private directory boundary are refused. Release
   only after the SSH child exits; cleanup is idempotent and cannot delete another
   lease. Terminal lifecycle owns process shutdown, draining and lease cleanup.
   Crash leftovers need private runtime-directory cleanup at service startup,
   after proving no old terminal processes remain; this module does not do that.
6. For Commit, prove the original VMM exited, copy the stopped disk, then call
   **`ScrubVM(ctx, sourceVMID, publicationCopy)`**, and publish only on success.
   It reads durable AES-GCM-protected installation metadata from the credential
   store, even after restart or removal of guest discovery metadata. It must not
   operate on the source disk if the source is to retain SSH access.
   Missing credentials/receipt, unsupported tools/account/path or symlinks are
   publication blockers. Legacy `Scrub(ctx, disk)` uses a guest discovery ID and
   is only a convenience for tests/inspection: a guest can remove that marker;
   **do not use it as the publication security boundary**.
7. Close/drain terminal children and release their leases before deleting the VM;
   after proven VM deletion call `Store.Delete(vmID)`. Retrieval remains possible
   until this call.

## Supported guest layout

* A directly addressable ext filesystem supported by installed e2fsprogs
  `debugfs` (tested with disposable ext4), not a partitioned whole-disk image,
  encrypted filesystem, overlay, mounted/live filesystem, or arbitrary format.
  Caller proves unmounted/exclusive ownership; the module cannot prove it.
* Real `/etc` directory, regular `/etc/passwd`, one configured guest account with
  numeric UID/GID and an absolute safe ASCII home. Tested `/root` and
  `/home/alice`; other real directory chains with the same layout work. Symlinks
  anywhere in `/etc/passwd` or home/`.ssh` ancestry are unsupported.
* Existing home directory. `.ssh` may be absent; `authorized_keys` may be absent
  or a regular file. Symlink files are refused, including at scrub time.
* Working preinstalled sshd accepting `%h/.ssh/authorized_keys`, public-key login
  enabled for this account, suitable shell/home ownership and no conflicting SSH
  policy or cloud-init rewrite. This module does not install, start, enable, or
  configure sshd and cannot establish these runtime facts offline.
* `/etc/vibe-fc-ssh-access` is reserved for the guest-side discovery VM ID. An
  unrelated preexisting file causes a conflict. Its contents are not trusted
  ownership evidence; ownership comes from the encrypted host credential record.

## Publication hygiene, not secret erasure

The trusted receipt records the exact path, exact generated key lines (including
VM/role comments), whether the file was created, and the original prefix needed
to undo an inserted newline. Scrub removes only those exact owned lines. Generic
`vibe-fc-managed:*` comments, preexisting identical supplied keys, unrelated bytes,
missing trailing newline and later legitimate entries are preserved. It preserves
current file owner/mode when rewriting and only removes a file it created when no
bytes remain. It does not remove the now-empty `.ssh` directory.

A guest can copy a key to another path, remove/change its comment, move its home,
modify discovery metadata, or deliberately leave other credentials. ScrubVM fails
closed for unsupported account/path changes, but **cannot discover or erase
arbitrary user-moved/modified copies**. It does not scan the guest filesystem,
revoke supplied keys elsewhere, erase private keys the user uploaded, guarantee
unallocated-block erasure, or sanitize other secrets. Even filesystem removal is
not forensic erasure. Publish only trusted guest content under this limited
managed-entry hygiene contract; do not advertise generic secret sanitization.

## Offline verification

Run from the repository, with no root or VMM:

```
GOMAXPROCS=2 go test -race -p 1 ./internal/sshaccess ./internal/localcfg ./vmlib/local ./internal/vmbot -count=1 -timeout=180s
```

Fixtures use genuine `mkfs.ext4 -d`, `debugfs`, and `e2fsck -fn`; the local backend
integration tests require these tools (the module-only tests skip if absent).
They exercise Prepare → cold Commit → derivative, restart-stable keys, supplied
keys, fail-closed scrub and failed/uncertain deletion without changing the base.
Terminal tests use an explicitly fake ttyd runner and verify dedicated argv and
key-file lifetime through process exit. This is offline filesystem and process
lifecycle evidence, not live SSH authentication or a real Firecracker boot.
