//go:build linux

package local_test

import (
	"fmt"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
	"testing"
)

func TestPublicSSHEndpointSlotMappingAndRestart(t *testing.T) {
	r := newRig(t, 4)
	r.cfg.PublicSSH = &vmlib.PublicSSHConfig{Host: "203.0.113.10", PortBase: 22000}
	b := r.backend()
	// Caller mutation cannot change operator policy after construction.
	r.cfg.PublicSSH.Host = "changed.example.test"
	for slot := 0; slot < 4; slot++ {
		id := fmt.Sprintf("public-%d", slot)
		h, err := prep(b, id)
		if err != nil {
			t.Fatal(err)
		}
		if h.GuestIP != fmt.Sprintf("172.30.%d.2", slot) || h.SSHEndpoint == nil || h.SSHEndpoint.Host != "203.0.113.10" || h.SSHEndpoint.Port != 22000+slot {
			t.Fatalf("slot %d: %+v", slot, h)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	r.cfg.PublicSSH.Host = "203.0.113.10"
	b = r.backend()
	svc, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	for slot := 0; slot < 4; slot++ {
		info, err := svc.SSHInfo(ctx, fmt.Sprintf("public-%d", slot))
		if err != nil {
			t.Fatal(err)
		}
		if info.Host != "203.0.113.10" || info.Port != 22000+slot || info.Bastion != nil {
			t.Fatalf("public metadata: %+v", info)
		}
		if info.InternalTarget == nil || info.InternalTarget.Host != fmt.Sprintf("172.30.%d.2", slot) || info.InternalTarget.Port != 22 {
			t.Fatalf("private metadata: %+v", info)
		}
	}
}

func TestPublicSSHDisabledPreservesLegacy(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	if _, err := prep(b, "legacy-public"); err != nil {
		t.Fatal(err)
	}
	svc, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	info, err := svc.SSHInfo(ctx, "legacy-public")
	if err != nil || info.Host != "172.30.0.2" || info.Port != 22 || info.Bastion == nil {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestPublicSSHInvalidRejectedBeforeOpeningRoots(t *testing.T) {
	r := newRig(t, 4)
	r.cfg.PublicSSH = &vmlib.PublicSSHConfig{Host: "public.example.test", PortBase: 65533}
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("accepted overflowing port range")
	}
}
