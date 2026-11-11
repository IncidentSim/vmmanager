//go:build linux

package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// OfflineAccess is optional host-owned access provisioning. No secret is carried
// in backend metadata or workflow results. Implementations must be replay-safe
// per VM ID. Install and ScrubVM may mutate only the private disk copy supplied.
// Delete is invoked only after workspace removal is proven durable. Callers must
// stop terminal sessions and release their file leases before Release.
type OfflineAccess interface {
	Install(ctx context.Context, vmID, disk, suppliedPublicKey string) error
	ScrubVM(ctx context.Context, vmID, disk string) error
	Delete(vmID string) error
}

func accessIntent(public string, enabled bool) string {
	if !enabled {
		return ""
	}
	h := sha256.Sum256([]byte(public))
	return hex.EncodeToString(h[:])
}
