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
)

func TestGracefulRetryAfterBootDropsFirstShutdown(t *testing.T) {
	for _, operation := range []string{"stop", "delete"} {
		t.Run(operation, func(t *testing.T) {
			r := newRig(t, 1)
			b := r.backend()
			s, err := vmlib.NewService(b)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Create(ctx, vmlib.CreateRequest{ID: "boot", ImageID: "base", Profile: "small"}); err != nil {
				t.Fatal(err)
			}
			r.vmm.onCtrl = func() {
				// The early boot guest ignores the first CAD. Only a later event exits.
				if r.vmm.ctrlAltDe == 2 {
					r.receipt(0, inv, "success", "exited", "0")
				}
			}
			c, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			started := time.Now()
			if operation == "stop" {
				vm, err := s.Stop(c, "boot")
				if err != nil || vm.State != vmlib.StateStopped {
					t.Fatalf("boot shutdown stranded: vm=%+v err=%v", vm, err)
				}
				if _, err := os.Stat(filepath.Join(r.root, "vms", "boot", "disk.ext4")); err != nil {
					t.Fatal(err)
				}
			} else {
				res, err := s.Delete(c, "boot")
				if err != nil || !res.Deleted {
					t.Fatalf("boot deletion stranded: result=%+v err=%v", res, err)
				}
			}
			r.vmm.mu.Lock()
			calls := r.vmm.ctrlAltDe
			r.vmm.mu.Unlock()
			if calls != 2 {
				t.Fatalf("shutdown actions=%d want 2", calls)
			}
			if time.Since(started) < time.Second {
				t.Fatal("shutdown retries must not spin")
			}
		})
	}
}

func TestGracefulRetryResumesExitingRetainedVM(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "boot")
	if _, err := b.RequestShutdown(ctx, "boot"); err != nil {
		t.Fatal(err)
	}
	// Reopening marks the unresolved exiting VM retained, as after a restart.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b = r.backend()
	h, err := b.Inspect(ctx, "boot")
	if err != nil || !h.Retained || h.State != vmlib.HostExiting {
		t.Fatalf("%+v %v", h, err)
	}
	r.vmm.onCtrl = func() { r.receipt(0, inv, "success", "exited", "0") }
	s, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	res, err := s.Delete(c, "boot")
	if err != nil || !res.Deleted {
		t.Fatalf("exiting retained VM stranded: %+v %v", res, err)
	}
}

func TestGracefulRetryRefusesReplacementAndRetainsDisk(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "boot")
	// Replacement appears after the first accepted (but ignored) action.
	r.vmm.onCtrl = func() { r.vmm.id = "ffffffffffffffffffffffffffffffff" }
	s, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := s.Delete(c, "boot")
	if !errors.Is(err, vmlib.ErrRetained) || !res.Retained || res.Deleted {
		t.Fatalf("%+v %v", res, err)
	}
	r.vmm.mu.Lock()
	calls := r.vmm.ctrlAltDe
	r.vmm.mu.Unlock()
	if calls != 1 {
		t.Fatalf("signalled replacement: %d actions", calls)
	}
	if _, err := os.Stat(filepath.Join(r.root, "vms", "boot", "disk.ext4")); err != nil {
		t.Fatal(err)
	}
	if _, err := prep(b, "other"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("uncertain exit freed lease: %v", err)
	}
	// A replacement's clean receipt is not proof of the original invocation.
	r.receipt(0, "ffffffffffffffffffffffffffffffff", "success", "exited", "0")
	if res, err := b.Release(ctx, "boot"); err != nil || res.Released {
		t.Fatalf("release with wrong receipt: %+v %v", res, err)
	}
	// Only the original receipt settles the retained VM, even though the
	// replacement still answers. No further action should reach that process.
	r.receipt(0, inv, "success", "exited", "0")
	if res, err := s.Delete(ctx, "boot"); err != nil || !res.Deleted {
		t.Fatalf("original receipt did not settle retention: %+v %v", res, err)
	}
	r.vmm.mu.Lock()
	calls = r.vmm.ctrlAltDe
	r.vmm.mu.Unlock()
	if calls != 1 {
		t.Fatalf("signalled replacement after original receipt: %d", calls)
	}
}

func TestGracefulRetryCancellationKeepsDiskAndLease(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "boot")
	s, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(ctx)
	r.vmm.onCtrl = cancel
	started := time.Now()
	vm, err := s.Stop(c, "boot")
	if !errors.Is(err, context.Canceled) || vm.State != vmlib.StateStopping {
		t.Fatalf("%+v %v", vm, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancellation was not prompt")
	}
	r.vmm.mu.Lock()
	calls := r.vmm.ctrlAltDe
	r.vmm.mu.Unlock()
	if calls != 1 {
		t.Fatalf("retried after cancellation: %d", calls)
	}
	if _, err := os.Stat(filepath.Join(r.root, "vms", "boot", "disk.ext4")); err != nil {
		t.Fatal(err)
	}
	if _, err := prep(b, "other"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("cancel freed lease: %v", err)
	}
}
