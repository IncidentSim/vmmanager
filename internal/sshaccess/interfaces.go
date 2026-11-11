package sshaccess

import "context"

// OfflineAccess is used only while the caller exclusively owns an unmounted,
// stopped per-VM working copy. Callers must discard a copy after any error.
// Install precedes first boot. ScrubVM precedes immutable image publication.
// Never invoke either against a base image or a disk held by a VMM.
type OfflineAccess interface {
	Install(ctx context.Context, vmID, disk, suppliedPublicKey string) error
	ScrubVM(ctx context.Context, vmID, disk string) error
}

// Credentials must remain outside durable workflow payloads/results. Authorize
// the VM owner before OwnerPrivateKey; revoke terminal sessions and release all
// file leases before Delete. Delete is called only after proven VM deletion.
type Credentials interface {
	Public(vmID string) (Public, error)
	OwnerPrivateKey(vmID string) ([]byte, error)
	TerminalKeyFile(vmID, runtimeDir string) (path string, release func(), err error)
	Delete(vmID string) error
}

var _ OfflineAccess = (*Provisioner)(nil)
var _ Credentials = (*Store)(nil)
