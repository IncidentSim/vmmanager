//go:build linux

package local_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

func TestDynamicResourcesExactRestartAdmission(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.UnitMemLimitMiB, r.cfg.MaxTotalMemMiB, r.cfg.MaxTotalVCPU = 1024, 1024, 4
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 3, MemMiB: 768, DiskMiB: 64}}
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := vmlib.VMResources{VCPU: 2, CPULimitMilli: 2000, MemMiB: 600}
	req := vmlib.PrepareRequest{VMID: "custom", ImageID: "base", Resources: &want}
	h, err := b.Prepare(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if h.Resources != want || h.Profile != "" {
		t.Fatalf("not exact: %+v", h)
	}
	if _, err := b.Prepare(ctx, req); err != nil {
		t.Fatal(err)
	}
	changed := vmlib.VMResources{VCPU: 1, MemMiB: 400}
	req.Resources = &changed
	if _, err := b.Prepare(ctx, req); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("changed reservation: %v", err)
	}
	b.Close()
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 256, DiskMiB: 64}}
	b, err = local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	h, err = b.Inspect(ctx, "custom")
	if err != nil || h.Resources != want {
		t.Fatalf("restart: %+v %v", h, err)
	}
	req.Resources = &want
	if _, err := b.Prepare(ctx, req); err != nil {
		t.Fatalf("exact retry after policy shrink must preserve admitted reservation: %v", err)
	}
	if _, err = b.Start(ctx, "custom"); err != nil {
		t.Fatal(err)
	}
	if got := r.vmm.specs[len(r.vmm.specs)-1]; got.VCPU != 2 || got.MemMiB != 600 {
		t.Fatalf("machine: %+v", got)
	}
	if _, err = prep(b, "next"); err != nil {
		t.Fatal(err)
	}
	if _, err = prep(b, "overflow"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("under-count: %v", err)
	}
}

func TestDynamicResourcesConcurrentAggregateAdmissionAndRelease(t *testing.T) {
	for _, budget := range []string{"memory", "cpu"} {
		t.Run(budget, func(t *testing.T) {
			r := newRig(t, 4)
			r.cfg.MaxTotalMemMiB = 512
			r.cfg.MaxTotalVCPU = 4
			if budget == "cpu" {
				r.cfg.MaxTotalMemMiB = 1024
				r.cfg.MaxTotalVCPU = 2
			}
			b, err := local.New(r.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			want := vmlib.VMResources{VCPU: 1, MemMiB: 193}
			var wg sync.WaitGroup
			ids := make(chan string, 12)
			errs := make(chan error, 12)
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					id := fmt.Sprintf("dynamic-%d", i)
					_, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: id, ImageID: "base", Resources: &want})
					if err == nil {
						ids <- id
					} else {
						errs <- err
					}
				}(i)
			}
			wg.Wait()
			close(ids)
			close(errs)
			if len(ids) != 2 {
				t.Fatalf("admitted %d, want exactly 2", len(ids))
			}
			for err := range errs {
				if !errors.Is(err, vmlib.ErrResourceExhausted) {
					t.Fatal(err)
				}
			}
			id := <-ids
			if _, err := b.Release(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "replacement", ImageID: "base", Resources: &want}); err != nil {
				t.Fatalf("released resources not reusable: %v", err)
			}
		})
	}
}

func TestDynamicResourcesRejectInvalidAndOutOfPolicy(t *testing.T) {
	r := newRig(t, 2)
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, res := range []vmlib.VMResources{{}, {VCPU: 1}, {MemMiB: 128}, {VCPU: 100, MemMiB: 128}, {VCPU: 1, MemMiB: 100000}} {
		if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "bad", ImageID: "base", Resources: &res}); !errors.Is(err, vmlib.ErrInvalidArgument) {
			t.Fatalf("%+v: %v", res, err)
		}
	}
	res := vmlib.VMResources{VCPU: 1, MemMiB: 128}
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "mixed", ImageID: "base", Profile: "small", Resources: &res}); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatal(err)
	}
}
