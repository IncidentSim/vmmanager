//go:build linux

package local

import "context"

// CooperativeShutdown may ask an authenticated managed guest to reboot gracefully.
// Implementations must honor context, use dedicated credentials and strict host
// authentication, and return only sanitized errors. Neither success nor failure
// proves that the original VMM exited. No lifecycle or release authority is given.
// Host is the directly reachable backend-derived guest IP, not a user target.
type CooperativeShutdown interface {
	Reboot(ctx context.Context, vmID, host, user string) error
}
