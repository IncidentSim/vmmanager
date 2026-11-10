//go:build linux

package localcfg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

type fakeLauncher struct{}

func (fakeLauncher) StartSlot(context.Context, int) error { return nil }

const invID = "0123456789abcdef0123456789abcdef"

type fakeVMM struct{ run string }

func (v fakeVMM) Info(context.Context, int) (local.VMMInfo, error) {
	return local.VMMInfo{ID: invID, State: "Not started"}, nil
}
func (v fakeVMM) Configure(context.Context, int, local.MachineSpec) error { return nil }
func (v fakeVMM) InstanceStart(context.Context, int) error                { return nil }
func (v fakeVMM) CtrlAltDel(_ context.Context, slot int) error {
	dir := filepath.Join(v.run, fmt.Sprintf("slot-%d", slot))
	os.MkdirAll(dir, 0o755)
	body := fmt.Sprintf(`{"invocation_id":%q,"result":"success","exit_code":"exited","exit_status":"0"}`, invID)
	return os.WriteFile(filepath.Join(dir, invID+".exit.json"), []byte(body), 0o640)
}

// The policy we SHIP must admit the two concurrent guests the integration flow
// needs (and refuse a third). It loads the real example file.
func TestShippedExamplePolicyAdmitsTwoSimultaneousVMsAndRejectsAThird(t *testing.T) {
	data, err := os.ReadFile("../../deployment/vmapi/backend.local.example.json")
	if err != nil {
		t.Fatal(err)
	}
	l, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	os.Chmod(d, 0o755)
	l.WorkRoot, l.ImagesDir, l.RunDir = filepath.Join(d, "work"), filepath.Join(d, "images"), filepath.Join(d, "run")
	l.StorageMount, l.PoolConf = "", "" // host-specific; covered by the backend tests
	for _, p := range []string{l.WorkRoot, l.ImagesDir, l.RunDir} {
		os.MkdirAll(p, 0o700)
	}
	os.WriteFile(filepath.Join(l.RunDir, ".backend.lock"), nil, 0o600)
	os.WriteFile(filepath.Join(l.ImagesDir, "base.ext4"), []byte("BASE"), 0o444)
	uid := uint32(os.Getuid())
	b, err := Open(l, Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{run: l.RunDir}, TrustedOwnerUID: &uid, TrustedAncestorRoot: d})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	for _, id := range []string{"v1", "v2"} {
		if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: id, ImageID: "base", Profile: l.Profiles[0].Name}); err != nil {
			t.Fatalf("shipped policy refuses concurrent VM %s: %v", id, err)
		}
	}
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "v3", ImageID: "base", Profile: l.Profiles[0].Name}); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("a third VM must be refused: %v", err)
	}
}

// Assembly carries every policy field into local.Config unchanged.
func TestAssemblePreservesEveryPolicyField(t *testing.T) {
	extra := `"ssh_user":"root","min_free_mib":7,"vmm_overhead_mib":65,"guest_dns":["1.1.1.1"],"bastion":{"host":"b","port":22,"user":"j"}`
	l, err := Parse([]byte(strings.Replace(good, `"ssh_user":"root"`, extra, 1)))
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.Assemble(Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}})
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkRoot != "/w" || c.ImagesDir != "/i" || c.RunDir != "/r" || c.MaxVMs != 2 || c.SSHUser != "root" ||
		c.MinFreeMiB != 7 || c.VMMOverheadMiB != 65 || c.StorageMount != "/var/lib/firecracker-builder/storage" ||
		c.MaxTotalMemMiB != 1024 || c.MaxTotalVCPU != 1 || c.UnitMemLimitMiB != 768 || c.GuestPrefix != "10.213" ||
		c.PoolConf != "/etc/firecracker-vms/pool.conf" || len(c.GuestDNS) != 1 || len(c.Profiles) != 1 ||
		c.Profiles[0] != (vmlib.Profile{Name: "small", VCPU: 1, MemMiB: 256, DiskMiB: 1024}) ||
		c.Bastion == nil || *c.Bastion != (vmlib.Bastion{Host: "b", Port: 22, User: "j"}) {
		t.Fatalf("lost policy: %+v", c)
	}
	if c.TrustedOwnerUID != nil || c.TrustedAncestorRoot != "" {
		t.Fatal("production assembly must not set test seams")
	}
}
