//go:build linux

package local_test

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"os"
	"testing"
)

type recordingNetwork struct {
	calls   int
	disk    string
	network vmlib.GuestNetwork
	fail    bool
}

func (p *recordingNetwork) InstallNetwork(_ context.Context, disk string, n vmlib.GuestNetwork) error {
	p.calls++
	p.disk = disk
	p.network = n
	if p.fail {
		return errors.New("unsupported image")
	}
	return nil
}
func TestGuestNetworkOnlyProvisionedOnNewPrivateCopy(t *testing.T) {
	r := newRig(t, 2)
	p := &recordingNetwork{}
	r.cfg.GuestNetwork = p
	r.cfg.GuestDNS = []string{"1.1.1.1"}
	b := r.backend()
	if _, err := prep(b, "network-copy"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 || p.disk != r.root+"/vms/network-copy/disk.ext4" || p.network.Gateway != "172.30.0.1" || p.network.Address != "172.30.0.2/30" || len(p.network.DNS) != 1 {
		t.Fatalf("bad network provision: %+v", p)
	}
	if _, err := prep(b, "network-copy"); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatal("reprovisioned existing disk")
	}
	p.fail = true
	if _, err := prep(b, "network-fail"); err == nil {
		t.Fatal("network failure was ignored")
	}
	if _, err := os.Stat(r.root + "/vms/network-fail"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed private copy retained as usable")
	}
}
