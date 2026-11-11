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

func TestPrepareReplayAfterProfileRemoval(t *testing.T) {
	r := newRig(t, 2)
	r.cfg.Profiles = []vmlib.Profile{half("small", 128)}
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	old, err := prep(b, "old")
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	r.cfg.Profiles = []vmlib.Profile{half("replacement", 256)}
	b, err = local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	got, err := prep(b, "old")
	if err != nil || got.Resources != old.Resources {
		t.Fatalf("replay: %+v %v", got, err)
	}
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "old", ImageID: "other", Profile: "small"}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("conflict: %v", err)
	}
	s, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := s.Capabilities(ctx)
	if err != nil || caps.DefaultProfile != "replacement" || caps.MaxTotalCPULimitMilli != r.cfg.MaxTotalVCPU*1000 {
		t.Fatalf("service caps: %+v %v", caps, err)
	}
}

func TestCorruptPersistedCPUReservationsBlockAdmissionAndStart(t *testing.T) {
	for name, fields := range map[string]map[string]uint32{
		"overflow legacy vcpu": {"vcpu": 4294968, "cpu_limit_milli": 0},
		"too many vcpus":       {"vcpu": 33},
		"unrepresentable":      {"cpu_limit_milli": 505},
		"above vcpu":           {"cpu_limit_milli": 1500},
		"above cpu maximum":    {"cpu_limit_milli": 4294967290},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, 2)
			b, err := local.New(r.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prep(b, "old"); err != nil {
				t.Fatal(err)
			}
			b.Close()
			path := filepath.Join(r.root, "vms", "old", "meta.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			for k, v := range fields {
				m[k] = v
			}
			raw, _ = json.Marshal(m)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			b, err = local.New(r.cfg)
			if err != nil {
				return
			}
			defer b.Close()
			if _, err := prep(b, "next"); err == nil {
				t.Error("admission accepted corrupt reservation")
			}
			if _, err := b.Start(ctx, "old"); err == nil {
				t.Error("started corrupt reservation")
			}
			if len(r.launcher.quotas) != 0 {
				t.Error("launcher called")
			}
		})
	}
}
