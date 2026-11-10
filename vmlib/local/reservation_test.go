//go:build linux

package local_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

// Finding 2: a VM keeps the memory/vCPU it was ADMITTED with, even if the
// operator later edits the profile of the same name.
func TestReservationsSurviveProfileEditAcrossRestart(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.UnitMemLimitMiB, r.cfg.MaxTotalMemMiB = 1024, 1024
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 768, DiskMiB: 64}}
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prep(b, "v1"); err != nil {
		t.Fatal(err)
	}
	b.Close()

	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 256, DiskMiB: 64}} // same name, smaller
	b2, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if _, err := prep(b2, "v2"); err != nil { // 768 + 256 = 1024
		t.Fatal(err)
	}
	if _, err := prep(b2, "v3"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("re-resolving the edited profile undercounted v1 (would admit 1280 MiB): %v", err)
	}
	// The reserved size, not today's profile, is what the VMM is configured with.
	if _, err := b2.Start(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if got := r.vmm.specs[len(r.vmm.specs)-1].MemMiB; got != 768 {
		t.Fatalf("started with %d MiB, reserved 768", got)
	}
}

func TestVCPUReservationsSurviveProfileEdit(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.MaxTotalVCPU = 4
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 3, MemMiB: 128, DiskMiB: 64}}
	b, _ := local.New(r.cfg)
	prep(b, "v1")
	b.Close()
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 128, DiskMiB: 64}}
	b2, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if _, err := prep(b2, "v2"); err != nil { // 3 + 1
		t.Fatal(err)
	}
	if _, err := prep(b2, "v3"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("vCPU reservation lost: %v", err)
	}
}

// Metadata written before reservations existed, or naming a profile that no
// longer exists, is counted at the LARGEST current profile, never as zero.
func TestLegacyOrUnknownProfileMetadataIsCountedConservatively(t *testing.T) {
	for _, mutate := range []func(m map[string]any){
		func(m map[string]any) { delete(m, "mem_mib"); delete(m, "vcpu") },
		func(m map[string]any) { delete(m, "mem_mib"); delete(m, "vcpu"); m["profile"] = "retired-profile" },
	} {
		r := newRig(t, 3)
		r.cfg.UnitMemLimitMiB, r.cfg.MaxTotalMemMiB = 1024, 1024
		r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 256, DiskMiB: 64}, {Name: "big", VCPU: 1, MemMiB: 512, DiskMiB: 64}}
		b, _ := local.New(r.cfg)
		if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "old", ImageID: "base", Profile: "small"}); err != nil {
			t.Fatal(err)
		}
		b.Close()
		path := filepath.Join(r.root, "vms", "old", "meta.json")
		raw, _ := os.ReadFile(path)
		var m map[string]any
		json.Unmarshal(raw, &m)
		mutate(m)
		raw, _ = json.Marshal(m)
		os.WriteFile(path, raw, 0o600)

		b2, err := local.New(r.cfg)
		if err != nil {
			t.Fatal(err)
		}
		// old is counted as 512 (the largest profile): 512 + 512 = 1024 fits, one more small does not.
		if _, err := b2.Prepare(ctx, vmlib.PrepareRequest{VMID: "n1", ImageID: "base", Profile: "big"}); err != nil {
			t.Fatal(err)
		}
		if _, err := b2.Prepare(ctx, vmlib.PrepareRequest{VMID: "n2", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrResourceExhausted) {
			t.Fatalf("legacy VM undercounted: %v", err)
		}
		b2.Close()
	}
}
