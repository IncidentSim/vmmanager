package vmbot

import (
	"context"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// DefaultProfile is the profile used for VMs whose record names none.
func (c *Controller) DefaultProfile() string { return c.cfg.Profile }

func (c *Controller) profileFor(r VMRecord) string {
	if r.Profile != "" {
		return r.Profile
	}
	return c.cfg.Profile
}

// ControlCapabilities returns the backend's live resource policy through the
// lifecycle facade. It is read-only: it performs no admission, ownership or
// workflow action and the caller must already be an authenticated allowed user.
func (c *Controller) ControlCapabilities(ctx context.Context, u int64) (vmlib.ResourceCapabilities, error) {
	if !c.ControlAllowed(u) {
		return vmlib.ResourceCapabilities{}, ErrControlForbidden
	}
	return c.lc.Capabilities(ctx)
}
