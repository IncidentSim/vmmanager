//go:build linux

package local_test

import (
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"testing"
)

func TestPublicSSHWithoutPoolRetainsHostnamePolicy(t *testing.T) {
	r := newRig(t, 1)
	r.cfg.PublicSSH = &vmlib.PublicSSHConfig{Host: "does-not-resolve.invalid", PortBase: 22000}
	b := r.backend()
	h, err := prep(b, "no-pool-hostname")
	if err != nil || h.SSHEndpoint == nil || h.SSHEndpoint.Host != "does-not-resolve.invalid" || h.SSHEndpoint.Port != 22000 {
		t.Fatalf("legacy no-pool policy: %+v %v", h, err)
	}
}
