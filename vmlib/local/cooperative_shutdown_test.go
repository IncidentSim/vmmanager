//go:build linux

package local_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

type shutdownAccess struct{}

func (shutdownAccess) Install(context.Context, string, string, string) error { return nil }
func (shutdownAccess) ScrubVM(context.Context, string, string) error         { return nil }
func (shutdownAccess) Delete(string) error                                   { return nil }

type cooperativeFunc func(context.Context, string, string, string) error

func (f cooperativeFunc) Reboot(c context.Context, id, host, user string) error {
	return f(c, id, host, user)
}

type failedCAD struct{ local.VMM }

func (v failedCAD) CtrlAltDel(c context.Context, slot int) error {
	_ = v.VMM.CtrlAltDel(c, slot)
	return errors.New("ambiguous CAD response")
}
func TestCooperativeShutdownCADErrorStillRetries(t *testing.T) {
	r := newRig(t, 1)
	r.cfg.Access = shutdownAccess{}
	r.cfg.VMM = failedCAD{r.vmm}
	calls := 0
	r.cfg.CooperativeShutdown = cooperativeFunc(func(context.Context, string, string, string) error {
		calls++
		if calls == 2 {
			r.receipt(0, inv, "success", "exited", "0")
		}
		return errors.New("SSH not ready or ambiguous")
	})
	b := r.backend()
	startRunning(t, r, b, "boot")
	s, _ := vmlib.NewService(b)
	c, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	res, err := s.Delete(c, "boot")
	if err != nil || !res.Deleted || calls != 2 {
		t.Fatalf("CAD error blocked fallback/retries: %+v %v calls=%d", res, err, calls)
	}
}

func TestCooperativeShutdownMissedCtrlRecovered(t *testing.T) {
	r := newRig(t, 1)
	r.cfg.Access = shutdownAccess{}
	calls := 0
	r.cfg.CooperativeShutdown = cooperativeFunc(func(c context.Context, id, host, user string) error {
		calls++
		if id != "boot" || host != "172.30.0.2" || user != "root" {
			t.Fatal("wrong target")
		}
		// Make-only CAD never produces an exit. SSH becomes ready on retry.
		if calls == 2 {
			r.receipt(0, inv, "success", "exited", "0")
		}
		return errors.New("ambiguous ack")
	})
	b := r.backend()
	startRunning(t, r, b, "boot")
	s, _ := vmlib.NewService(b)
	c, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	res, err := s.Delete(c, "boot")
	if err != nil || !res.Deleted || calls != 2 {
		t.Fatalf("%+v %v calls=%d", res, err, calls)
	}
}

func TestCooperativeShutdownIdentityAndReceiptFence(t *testing.T) {
	for _, mode := range []string{"legacy", "disabled", "replacement-before", "replacement-after-cad", "replacement-after-ssh", "ssh-unavailable", "ssh-ack"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, 1)
			if mode != "legacy" {
				r.cfg.Access = shutdownAccess{}
			}
			calls := 0
			if mode != "disabled" {
				r.cfg.CooperativeShutdown = cooperativeFunc(func(context.Context, string, string, string) error {
					calls++
					if mode == "replacement-after-ssh" {
						r.vmm.mu.Lock()
						r.vmm.id = "ffffffffffffffffffffffffffffffff"
						r.vmm.mu.Unlock()
					}
					if mode == "ssh-ack" {
						return nil
					}
					return errors.New("hostkey mismatch or not ready")
				})
			}
			b := r.backend()
			startRunning(t, r, b, "boot")
			if mode == "replacement-before" {
				r.vmm.id = "ffffffffffffffffffffffffffffffff"
			}
			if mode == "replacement-after-cad" {
				r.vmm.onCtrl = func() { r.vmm.id = "ffffffffffffffffffffffffffffffff" }
			}
			_, err := b.RequestShutdown(ctx, "boot")
			replaced := mode == "replacement-before" || mode == "replacement-after-cad" || mode == "replacement-after-ssh"
			if replaced && err == nil {
				t.Fatal("identity mismatch hidden")
			}
			want := 1
			if mode == "legacy" || mode == "disabled" || mode == "replacement-before" || mode == "replacement-after-cad" {
				want = 0
			}
			if calls != want {
				t.Fatalf("SSH calls=%d want %d", calls, want)
			}
			if res, e := b.Release(ctx, "boot"); e != nil || res.Released {
				t.Fatalf("unproven exit released: %+v %v", res, e)
			}
			if _, e := os.Stat(filepath.Join(r.root, "vms", "boot", "disk.ext4")); e != nil {
				t.Fatal(e)
			}
			r.receipt(0, inv, "success", "exited", "0")
			if res, e := b.Release(ctx, "boot"); e != nil || !res.Released {
				t.Fatalf("original receipt not accepted: %+v %v", res, e)
			}
		})
	}
}
