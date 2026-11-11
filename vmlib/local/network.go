//go:build linux

package local

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// OfflineGuestNetwork configures a fresh, exclusively owned private disk copy
// before its first boot. Never called on an existing/running disk or base image.
// Failure discards the copy; it cannot grant forwarding authority to a client.
type OfflineGuestNetwork interface {
	InstallNetwork(ctx context.Context, disk string, network vmlib.GuestNetwork) error
}
