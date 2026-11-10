package remote_test

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"testing"
)

func TestDynamicResourcesAndDeleteImageAcrossBothGRPCHops(t *testing.T) {
	e := startBackend(t)
	bc := connect(t, e)
	svc, err := vmlib.NewService(bc)
	if err != nil {
		t.Fatal(err)
	}
	c := facadeClient(t, serveFacade(t, svc))
	ctx := context.Background()
	want := vmlib.VMResources{VCPU: 1, MemMiB: 321}
	v, err := c.Create(ctx, vmlib.CreateRequest{ID: "custom", ImageID: "base", Resources: &want})
	if err != nil || v.Resources != want {
		t.Fatalf("create: %+v %v", v, err)
	}
	h, err := bc.Inspect(ctx, "custom")
	if err != nil || h.Resources != want {
		t.Fatalf("backend: %+v %v", h, err)
	}
	if v, err := c.Get(ctx, "custom"); err != nil || v.Resources != want {
		t.Fatalf("get: %+v %v", v, err)
	}
	if _, err := c.Create(ctx, vmlib.CreateRequest{ID: "mixed", ImageID: "base", Profile: "small", Resources: &want}); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatalf("mixed: %v", err)
	}
	if _, err := c.Commit(ctx, "custom", "golden"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Create(ctx, vmlib.CreateRequest{ID: "consumer", ImageID: "golden", Resources: &want}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteImage(ctx, "golden"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("reference: %v", err)
	}
	if _, err := c.Delete(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteImage(ctx, "golden"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteImage(ctx, "golden"); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.GetImage(ctx, "golden"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatal(err)
	}
	if err := c.DeleteImage(ctx, "base"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("base: %v", err)
	}
}
