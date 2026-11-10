//go:build linux

package local

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type nopLauncher struct{}

func (nopLauncher) StartSlot(context.Context, int) error { return nil }

type nopVMM struct{}

func (nopVMM) Info(context.Context, int) (VMMInfo, error) {
	return VMMInfo{ID: "0123456789abcdef0123456789abcdef", State: "Not started"}, nil
}
func (nopVMM) Configure(context.Context, int, MachineSpec) error { return nil }
func (nopVMM) InstanceStart(context.Context, int) error          { return nil }
func (nopVMM) CtrlAltDel(context.Context, int) error             { return nil }

func abaBackend(t *testing.T) (*Backend, string) {
	t.Helper()
	d := t.TempDir()
	cfg := Config{WorkRoot: filepath.Join(d, "w"), ImagesDir: filepath.Join(d, "i"), RunDir: filepath.Join(d, "r"), MaxVMs: 2,
		Profiles: []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 128, DiskMiB: 8}},
		Launcher: nopLauncher{}, VMM: nopVMM{}, MaxTotalMemMiB: 1024, MaxTotalVCPU: 8, UnitMemLimitMiB: 768, VMMOverheadMiB: 64, GuestPrefix: "172.30",
		CopyFile: func(src, dst string) error { return os.WriteFile(dst, []byte("disk"), 0o600) },
	}
	for _, p := range []string{cfg.WorkRoot, cfg.ImagesDir, cfg.RunDir} {
		os.MkdirAll(p, 0o700)
	}
	os.WriteFile(filepath.Join(cfg.RunDir, ".backend.lock"), nil, 0o600)
	uid := uint32(os.Getuid())
	cfg.TrustedOwnerUID = &uid
	os.Chmod(d, 0o755)
	cfg.TrustedAncestorRoot = d
	os.WriteFile(filepath.Join(cfg.ImagesDir, "base.ext4"), []byte("b"), 0o444)
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, cfg.WorkRoot
}

// A delayed second Release that looked up the OLD entry must never delete a
// workspace that was re-created under the same ID in the meantime.
func TestStaleReleaseCannotDeleteReusedID(t *testing.T) {
	b, work := abaBackend(t)
	ctx := context.Background()
	req := vmlib.PrepareRequest{VMID: "a", ImageID: "base", Profile: "small"}
	if _, err := b.Prepare(ctx, req); err != nil {
		t.Fatal(err)
	}
	paused, gate := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	armed := true
	b.hookAfterLookup = func(id string) {
		mu.Lock()
		hit := armed
		armed = false
		mu.Unlock()
		if hit {
			close(paused)
			<-gate
		}
	}
	done := make(chan vmlib.ReleaseResult, 1)
	go func() { r, _ := b.Release(ctx, "a"); done <- r }() // B: holds the old entry
	<-paused
	// A: a full release and a new VM under the same ID.
	if r, err := b.Release(ctx, "a"); err != nil || !r.Released {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := b.Prepare(ctx, req); err != nil {
		t.Fatal(err)
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stale release hung")
	}
	if _, err := os.Stat(filepath.Join(work, "vms", "a", "disk.ext4")); err != nil {
		t.Fatal("stale Release deleted the NEW VM's workspace")
	}
	if h, err := b.Inspect(ctx, "a"); err != nil || h.State != vmlib.HostPrepared {
		t.Fatalf("stale Release removed the NEW VM's lease: %v %+v", err, h)
	}
}
