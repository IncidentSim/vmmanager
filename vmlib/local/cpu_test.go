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

func half(name string, mem uint32) vmlib.Profile {
	return vmlib.Profile{Name: name, VCPU: 1, CPULimitMilli: 500, MemMiB: mem, DiskMiB: 64}
}

func TestStartHandsTheReservedCPUQuotaToTheLauncher(t *testing.T) {
	r := newRig(t, 2)
	r.cfg.Profiles = []vmlib.Profile{half("small", 256)}
	b := r.backend()
	h, err := prep(b, "a")
	if err != nil || h.Resources != (vmlib.VMResources{VCPU: 1, CPULimitMilli: 500, MemMiB: 256}) {
		t.Fatalf("%v %+v", err, h.Resources)
	}
	if _, err := b.Start(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if len(r.launcher.quotas) != 1 || r.launcher.quotas[0] != 500 {
		t.Fatalf("launcher saw quotas %v; the limit must be passed to the privileged helper, not just advertised", r.launcher.quotas)
	}
	if got := r.vmm.specs[0]; got.VCPU != 1 || got.MemMiB != 256 {
		t.Fatalf("guest must still get whole vCPUs: %+v", got)
	}
}

func TestCPUReservationSurvivesProfileEditAndRestart(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.MaxTotalCPULimitMilli = 990
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, CPULimitMilli: 800, MemMiB: 128, DiskMiB: 64}}
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prep(b, "v1"); err != nil {
		t.Fatal(err)
	}
	b.Close()

	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, CPULimitMilli: 100, MemMiB: 128, DiskMiB: 64}} // same name, far smaller
	b2, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if _, err := prep(b2, "v2"); err != nil { // 800 + 100 <= 1000
		t.Fatal(err)
	}
	if _, err := prep(b2, "v3"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("v1's 800m reservation was re-resolved to the edited profile: %v", err)
	}
	if _, err := b2.Start(ctx, "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.Start(ctx, "v2"); err != nil {
		t.Fatal(err)
	}
	if q := r.launcher.quotas; len(q) != 2 || q[0] != 800 || q[1] != 100 {
		t.Fatalf("quotas %v; v1 must keep the 800m it was admitted with", q)
	}
}

func TestAggregateCPUBudgetIsEnforcedIndependentlyOfVCPU(t *testing.T) {
	r := newRig(t, 4)
	r.cfg.Profiles = []vmlib.Profile{half("small", 128)}
	r.cfg.MaxTotalVCPU = 4
	r.cfg.MaxTotalCPULimitMilli = 1000 // two halves
	b := r.backend()
	for _, id := range []string{"a", "b"} {
		if _, err := prep(b, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prep(b, "c"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("a third 0.5 CPU VM fits 3 vCPUs but not 1.0 CPU of time: %v", err)
	}
	caps, _ := b.Capabilities(ctx)
	if caps.MaxTotalCPULimitMilli != 1000 || caps.Profiles[0].CPULimitMilli != 500 {
		t.Fatalf("%+v", caps)
	}
	// Four slots at 0.5 CPU fit when the budget says so, the deployment's target.
	r2 := newRig(t, 4)
	r2.cfg.Profiles = []vmlib.Profile{half("small", 500)}
	r2.cfg.MaxTotalVCPU, r2.cfg.MaxTotalCPULimitMilli, r2.cfg.MaxTotalMemMiB = 4, 2000, 2000
	b2 := r2.backend()
	for _, id := range []string{"a", "b", "c", "d"} {
		if _, err := prep(b2, id); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, err := prep(b2, "e"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatal(err)
	}
}

func TestMissingAggregateCPULimitDefaultsToTheVCPUBudget(t *testing.T) {
	r := newRig(t, 4)
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 128, DiskMiB: 64}} // pre-cpu_limit policy
	r.cfg.MaxTotalVCPU = 2
	b := r.backend()
	caps, _ := b.Capabilities(ctx)
	if caps.MaxTotalCPULimitMilli != 2000 || caps.Profiles[0].CPULimitMilli != 1000 {
		t.Fatalf("legacy policy must mean the full vCPU allowance, not an uncapped value: %+v", caps)
	}
	prep(b, "a")
	prep(b, "b")
	if _, err := prep(b, "c"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatal(err)
	}
	b.Start(ctx, "a")
	if r.launcher.quotas[0] != 1000 {
		t.Fatalf("legacy profile must still start with a concrete quota: %v", r.launcher.quotas)
	}
}

func TestInvalidCPUPolicyIsRefusedAtStartup(t *testing.T) {
	for name, p := range map[string]vmlib.Profile{
		"unrepresentable": {Name: "small", VCPU: 1, CPULimitMilli: 505, MemMiB: 128, DiskMiB: 64},
		"above vcpu":      {Name: "small", VCPU: 1, CPULimitMilli: 1500, MemMiB: 128, DiskMiB: 64},
		"above aggregate": {Name: "small", VCPU: 8, CPULimitMilli: 8000, MemMiB: 128, DiskMiB: 64},
	} {
		r := newRig(t, 2)
		r.cfg.MaxTotalVCPU, r.cfg.MaxTotalCPULimitMilli = 16, 4000
		r.cfg.Profiles = []vmlib.Profile{p}
		if b, err := local.New(r.cfg); err == nil {
			b.Close()
			t.Errorf("%s accepted", name)
		}
	}
	r := newRig(t, 2)
	r.cfg.MaxTotalCPULimitMilli = 5 // not a whole percent
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Error("unrepresentable aggregate accepted")
	}
}

func TestLegacyMetadataWithoutCPULimitIsCountedAndStartedAtItsVCPUAllowance(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.MaxTotalVCPU, r.cfg.MaxTotalCPULimitMilli = 4, 2000
	r.cfg.Profiles = []vmlib.Profile{half("small", 128)}
	b, _ := local.New(r.cfg)
	if _, err := prep(b, "old"); err != nil {
		t.Fatal(err)
	}
	b.Close()
	path := filepath.Join(r.root, "vms", "old", "meta.json")
	raw, _ := os.ReadFile(path)
	var m map[string]any
	json.Unmarshal(raw, &m)
	if m["cpu_limit_milli"] != float64(500) || m["mem_mib"] != float64(128) {
		t.Fatalf("effective CPU+RAM must both be persisted (mem under its existing private name): %v", m)
	}
	delete(m, "cpu_limit_milli")
	raw, _ = json.Marshal(m)
	os.WriteFile(path, raw, 0o600)

	b2, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	h, _ := b2.Inspect(ctx, "old")
	if h.Resources.CPULimitMilli != 1000 {
		t.Fatalf("pre-limit VM must count at 1 vCPU = 1000m, got %+v", h.Resources)
	}
	prep(b2, "n1")                            // 1000 + 500
	if _, err := prep(b2, "n2"); err != nil { // 1500 + 500 = 2000
		t.Fatal(err)
	}
	if _, err := prep(b2, "n3"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("legacy VM undercounted at 500m: %v", err)
	}
	b2.Start(ctx, "old")
	if r.launcher.quotas[0] != 1000 {
		t.Fatal(r.launcher.quotas)
	}
}

func TestExplicitResourcesCarryOrDefaultTheirCPULimit(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.MaxTotalVCPU, r.cfg.MaxTotalCPULimitMilli = 8, 4000
	r.cfg.Profiles = []vmlib.Profile{{Name: "big", VCPU: 2, CPULimitMilli: 1500, MemMiB: 512, DiskMiB: 64}}
	b := r.backend()
	h, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "x", ImageID: "base", Resources: &vmlib.VMResources{VCPU: 1, CPULimitMilli: 300, MemMiB: 128}})
	if err != nil || h.Resources.CPULimitMilli != 300 {
		t.Fatalf("%v %+v", err, h.Resources)
	}
	// Same ID, different CPU limit is a conflict, not an idempotent replay.
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "x", ImageID: "base", Resources: &vmlib.VMResources{VCPU: 1, CPULimitMilli: 400, MemMiB: 128}}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("%v", err)
	}
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "x", ImageID: "base", Resources: &vmlib.VMResources{VCPU: 1, CPULimitMilli: 300, MemMiB: 128}}); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	for name, res := range map[string]vmlib.VMResources{
		"above profile max": {VCPU: 2, CPULimitMilli: 1510, MemMiB: 128},
		"unrepresentable":   {VCPU: 1, CPULimitMilli: 505, MemMiB: 128},
		"above own vcpu":    {VCPU: 1, CPULimitMilli: 1500, MemMiB: 128},
	} {
		res := res
		if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "y", ImageID: "base", Resources: &res}); !errors.Is(err, vmlib.ErrInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Omitted limit means the full vCPU allowance, still bounded by the profile maximum.
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "z", ImageID: "base", Resources: &vmlib.VMResources{VCPU: 1, MemMiB: 128}}); err != nil {
		t.Fatal(err)
	}
	b.Start(ctx, "z")
	if r.launcher.quotas[0] != 1000 {
		t.Fatal(r.launcher.quotas)
	}
}

func TestAggregateBudgetIsNotLimitedToOneVM(t *testing.T) {
	r := newRig(t, 4)
	r.cfg.MaxTotalVCPU = 128
	if b, err := local.New(r.cfg); err != nil {
		t.Fatal(err)
	} else {
		b.Close()
	}
}

func TestDuplicateProfileIsRejected(t *testing.T) {
	r := newRig(t, 2)
	r.cfg.Profiles = []vmlib.Profile{half("small", 128), half("small", 256)}
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("ambiguous catalogue accepted")
	}
}

func TestPoolCPUCeilingIsCheckedAtStartup(t *testing.T) {
	r := newRig(t, 2)
	p := filepath.Join(r.root, "pool.conf")
	if err := os.WriteFile(p, []byte("GUEST_PREFIX=172.30\nMAX_SLOTS=2\nMAX_SLOT_CPU_MILLI=250\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r.cfg.PoolConf = p
	r.cfg.Profiles = []vmlib.Profile{half("small", 128)}
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("unlaunchable profile accepted")
	}
}

func TestCapabilitiesDefaultProfile(t *testing.T) {
	r := newRig(t, 2)
	r.cfg.Profiles = []vmlib.Profile{half("tiny", 128), half("small", 256)}
	r.cfg.DefaultProfile = "small"
	caps, _ := r.backend().Capabilities(ctx)
	if caps.DefaultProfile != "small" {
		t.Fatal(caps.DefaultProfile)
	}
	r.cfg.DefaultProfile = "nope"
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("unknown default profile accepted")
	}
}
