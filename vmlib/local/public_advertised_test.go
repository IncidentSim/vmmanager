//go:build linux

package local_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

func TestPublicSSHAdvertisedHostIsPresentationOnly(t *testing.T) {
	r := newRig(t, 4)
	r.cfg.PoolConf = filepath.Join(t.TempDir(), "pool.conf")
	if err := os.WriteFile(r.cfg.PoolConf, []byte("GUEST_PREFIX=172.30\nMAX_SLOTS=4\nPUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b := r.backend() // no advertised host: backward-compatible literal
	h, err := prep(b, "adv")
	if err != nil || h.SSHEndpoint == nil || h.SSHEndpoint.Host != "8.8.8.8" {
		t.Fatalf("default endpoint: %+v %v", h, err)
	}
	b.Close()

	// An existing VM is reported with the advertised name; port/slot unchanged.
	r.cfg.PublicSSH = &vmlib.PublicSSHConfig{Host: "8.8.8.8", PortBase: 22000, AdvertisedHost: "rkn.petruekhin.tech"}
	b = r.backend()
	info, err := b.Inspect(ctx, "adv")
	if err != nil || info.SSHEndpoint == nil || info.SSHEndpoint.Host != "rkn.petruekhin.tech" || info.SSHEndpoint.Port != 22000 {
		t.Fatalf("advertised endpoint: %+v %v", info, err)
	}
	b.Close()

	// Pool authority is not relaxed: Host must remain the root literal.
	for _, p := range []*vmlib.PublicSSHConfig{
		{Host: "rkn.petruekhin.tech", PortBase: 22000, AdvertisedHost: "rkn.petruekhin.tech"},
		{Host: "8.8.4.4", PortBase: 22000, AdvertisedHost: "rkn.petruekhin.tech"},
		{Host: "8.8.8.8", PortBase: 22001, AdvertisedHost: "rkn.petruekhin.tech"},
		{Host: "8.8.8.8", PortBase: 22000, AdvertisedHost: "bad host"},
	} {
		r.cfg.PublicSSH = p
		if b, err := local.New(r.cfg); err == nil {
			b.Close()
			t.Fatalf("accepted %+v", p)
		}
	}
}
