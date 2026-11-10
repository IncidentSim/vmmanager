//go:build linux

package local_test

import (
	"errors"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Uses the real local disk/catalog/metadata logic behind vmlib.Service in the
// SAME process (exactly how vmapi embeds it; there is no transport). Only the
// launcher and Firecracker API are fakes; no host VM is started.
func TestLocalBackendDynamicResourcesThroughEmbeddedService(t *testing.T) {
	r := newRig(t, 2)
	bc := r.backend()
	fc, err := vmlib.NewService(bc)
	if err != nil {
		t.Fatal(err)
	}
	want := vmlib.VMResources{VCPU: 1, MemMiB: 193}
	v, err := fc.Create(ctx, vmlib.CreateRequest{ID: "custom", ImageID: "base", Resources: &want})
	if err != nil || v.Resources != want {
		t.Fatalf("create: %+v %v", v, err)
	}
	h, err := bc.Inspect(ctx, "custom")
	if err != nil || h.Resources != want {
		t.Fatalf("inspect: %+v %v", h, err)
	}
	if got := r.vmm.specs[0]; got.VCPU != want.VCPU || got.MemMiB != want.MemMiB {
		t.Fatalf("machine spec: %+v", got)
	}
	r.receipt(0, inv, "success", "exited", "0")
	if _, err := fc.Commit(ctx, "custom", "derived"); err != nil {
		t.Fatal(err)
	}
	if _, err := fc.Create(ctx, vmlib.CreateRequest{ID: "legacy", ImageID: "derived", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	if v, err := fc.Get(ctx, "legacy"); err != nil || v.Resources.MemMiB != 256 {
		t.Fatalf("legacy effective resources: %+v %v", v, err)
	}
	if err := fc.DeleteImage(ctx, "derived"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("reference guard: %v", err)
	}
	r.receipt(1, inv, "success", "exited", "0")
	if _, err := fc.Delete(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if err := fc.DeleteImage(ctx, "derived"); err != nil {
		t.Fatal(err)
	}
	if err := fc.DeleteImage(ctx, "derived"); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.GetImage(ctx, "derived"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("readback: %v", err)
	}
	if err := fc.DeleteImage(ctx, "base"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("base: %v", err)
	}
}
