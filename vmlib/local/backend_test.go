//go:build linux

package local_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"example.com/firecracker-postgres-builder/vmlib"
	"example.com/firecracker-postgres-builder/vmlib/local"
)

const inv = "0123456789abcdef0123456789abcdef"

// fakes for the two narrow seams. They are NOT evidence of real Firecracker.
type fakeLauncher struct {
	mu    sync.Mutex
	slots []int
	err   error
}

func (l *fakeLauncher) StartSlot(_ context.Context, slot int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.slots = append(l.slots, slot)
	return l.err
}

type fakeVMM struct {
	mu        sync.Mutex
	state     string
	id        string
	specs     []local.MachineSpec
	ctrlAltDe int
	cfgErr    error
	infoErr   error
}

func (v *fakeVMM) Info(context.Context, int) (local.VMMInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.infoErr != nil {
		return local.VMMInfo{}, v.infoErr
	}
	return local.VMMInfo{ID: v.id, State: v.state}, nil
}
func (v *fakeVMM) Configure(_ context.Context, _ int, s local.MachineSpec) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.specs = append(v.specs, s)
	return v.cfgErr
}

// InstanceStart keeps the shared fake in "Not started" so several slots can be
// started in one test; tests that need a busy VMM set state explicitly.
func (v *fakeVMM) InstanceStart(context.Context, int) error { return nil }
func (v *fakeVMM) CtrlAltDel(context.Context, int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ctrlAltDe++
	return nil
}

type rig struct {
	t        *testing.T
	root     string
	images   string
	run      string
	launcher *fakeLauncher
	vmm      *fakeVMM
	cfg      local.Config
}

func newRig(t *testing.T, max int) *rig {
	t.Helper()
	d := t.TempDir()
	r := &rig{t: t, root: filepath.Join(d, "work"), images: filepath.Join(d, "images"), run: filepath.Join(d, "run"),
		launcher: &fakeLauncher{}, vmm: &fakeVMM{state: "Not started", id: inv}}
	for _, p := range []string{r.root, r.images, r.run} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r.images, "base.ext4"), []byte("BASE-IMAGE-CONTENT"), 0o444); err != nil {
		t.Fatal(err)
	}
	r.cfg = local.Config{
		WorkRoot: r.root, ImagesDir: r.images, RunDir: r.run, MaxVMs: max,
		Profiles: []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 256, DiskMiB: 64}},
		SSHUser:  "root", Bastion: &vmlib.Bastion{Host: "bastion.example.test", Port: 22, User: "jump"},
		Launcher: r.launcher, VMM: r.vmm, MinFreeMiB: 0,
	}
	return r
}

func (r *rig) backend() *local.Backend {
	b, err := local.New(r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

func (r *rig) receipt(slot int, id, result, code, status string) {
	dir := filepath.Join(r.run, fmt.Sprintf("slot-%d", slot))
	os.MkdirAll(dir, 0o755)
	body := fmt.Sprintf(`{"invocation_id":%q,"result":%q,"exit_code":%q,"exit_status":%q}`, id, result, code, status)
	if err := os.WriteFile(filepath.Join(dir, id+".exit.json"), []byte(body), 0o640); err != nil {
		r.t.Fatal(err)
	}
}

func sum(t *testing.T, p string) string {
	d, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(d)
	return hex.EncodeToString(h[:])
}

var ctx = context.Background()

func prep(b *local.Backend, id string) (vmlib.HostVM, error) {
	return b.Prepare(ctx, vmlib.PrepareRequest{VMID: id, ImageID: "base", Profile: "small"})
}

func TestPrepareCopiesImageAndNeverTouchesBase(t *testing.T) {
	r := newRig(t, 2)
	before := sum(t, filepath.Join(r.images, "base.ext4"))
	b := r.backend()
	h, err := prep(b, "a")
	if err != nil || h.State != vmlib.HostPrepared || h.GuestIP != "172.17.0.2" {
		t.Fatalf("%v %+v", err, h)
	}
	disk := filepath.Join(r.root, "vms", "a", "disk.ext4")
	if sum(t, disk) != before {
		t.Fatal("workspace copy differs from base")
	}
	os.WriteFile(disk, []byte("mutated"), 0o600)
	if sum(t, filepath.Join(r.images, "base.ext4")) != before {
		t.Fatal("base image modified")
	}
	if again, err := prep(b, "a"); err != nil || again.ID != "a" {
		t.Fatalf("idempotent prepare: %v", err)
	}
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "a", ImageID: "base", Profile: "other"}); err == nil {
		t.Fatal("conflicting prepare accepted")
	}
}

func TestPrepareRejectsBadInputs(t *testing.T) {
	r := newRig(t, 2)
	os.Symlink("/etc/passwd", filepath.Join(r.images, "evil.ext4"))
	os.WriteFile(filepath.Join(r.images, "huge.ext4"), make([]byte, 65<<20), 0o444)
	b := r.backend()
	cases := map[string]vmlib.PrepareRequest{
		"traversal vm":   {VMID: "../x", ImageID: "base", Profile: "small"},
		"path image":     {VMID: "v", ImageID: "/etc/passwd", Profile: "small"},
		"symlink image":  {VMID: "v", ImageID: "evil", Profile: "small"},
		"oversize image": {VMID: "v", ImageID: "huge", Profile: "small"},
		"profile":        {VMID: "v", ImageID: "base", Profile: "gigantic"},
		"key":            {VMID: "v", ImageID: "base", Profile: "small", SSHPublicKey: "ssh-ed25519 AAAA test"},
	}
	for name, req := range cases {
		if _, err := b.Prepare(ctx, req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "v", ImageID: "nosuch", Profile: "small"}); !errors.Is(err, vmlib.ErrNotFound) {
		t.Errorf("missing image: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(r.root, "vms"))
	if len(entries) != 0 {
		t.Fatalf("rejected requests left workspace state: %v", entries)
	}
}

func TestParallelPrepareIsCapBoundedWithUniqueSlots(t *testing.T) {
	r := newRig(t, 3)
	b := r.backend()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ips, ok := map[string]bool{}, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := prep(b, fmt.Sprintf("v%d", i))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
				if ips[h.GuestIP] {
					t.Errorf("duplicate IP %s", h.GuestIP)
				}
				ips[h.GuestIP] = true
			} else if !errors.Is(err, vmlib.ErrResourceExhausted) {
				t.Errorf("%v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != 3 {
		t.Fatalf("admitted %d, cap 3", ok)
	}
}

func TestStartConfiguresDerivedResourcesOnly(t *testing.T) {
	r := newRig(t, 2)
	b := r.backend()
	prep(b, "a")
	prep(b, "b")
	h, err := b.Start(ctx, "b")
	if err != nil || h.State != vmlib.HostRunning {
		t.Fatalf("%v %+v", err, h)
	}
	if len(r.launcher.slots) != 1 || r.launcher.slots[0] != 1 {
		t.Fatalf("launcher slots %v", r.launcher.slots)
	}
	s := r.vmm.specs[0]
	if s.Tap != "tap-vm1" || s.MAC != "06:00:AC:11:01:02" || s.DiskJailPath != "/work/vms/b/disk.ext4" ||
		s.Kernel != "/kernel/vmlinux" || s.VCPU != 1 || s.MemMiB != 256 {
		t.Fatalf("%+v", s)
	}
}

func TestStartRefusesBusyOrUnidentifiedVMM(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	prep(b, "a")
	r.vmm.state = "Running" // someone else's guest
	if _, err := b.Start(ctx, "a"); err == nil {
		t.Fatal("started on a busy VMM")
	}
	if len(r.vmm.specs) != 0 {
		t.Fatal("configured a busy VMM")
	}
	r.vmm.state, r.vmm.id = "Not started", "not-hex"
	if _, err := b.Start(ctx, "a"); err == nil || len(r.vmm.specs) != 0 {
		t.Fatal("accepted invalid invocation id")
	}
	h, _ := b.Inspect(ctx, "a")
	if h.State != vmlib.HostPrepared || h.Retained {
		t.Fatalf("pre-configuration refusal must stay retryable: %+v", h)
	}
}

func TestConfigureFailureRetainsDisk(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	prep(b, "a")
	r.vmm.cfgErr = errors.New("socket reset")
	if _, err := b.Start(ctx, "a"); err == nil {
		t.Fatal("expected error")
	}
	h, _ := b.Inspect(ctx, "a")
	if !h.Retained || h.State != vmlib.HostRetained {
		t.Fatalf("%+v", h)
	}
	if res, _ := b.Release(ctx, "a"); res.Released {
		t.Fatal("released a disk that a VMM may hold")
	}
	if _, err := os.Stat(filepath.Join(r.root, "vms", "a", "disk.ext4")); err != nil {
		t.Fatal("disk removed")
	}
}

func startRunning(t *testing.T, r *rig, b *local.Backend, id string) {
	t.Helper()
	if _, err := prep(b, id); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownRefusesReplacementIdentity(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "a")
	r.vmm.id = "fedcba9876543210fedcba9876543210" // a replacement VMM answers
	if _, err := b.RequestShutdown(ctx, "a"); err == nil || r.vmm.ctrlAltDe != 0 {
		t.Fatalf("sent action to a replacement VMM: %v", err)
	}
	r.vmm.id = inv
	if _, err := b.RequestShutdown(ctx, "a"); err != nil || r.vmm.ctrlAltDe != 1 {
		t.Fatalf("%v", err)
	}
}

func TestWaitExitNeedsOriginalReceiptNotResponsiveSocket(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "a")
	// A replacement VMM responds, with another receipt for a different id.
	r.receipt(0, "ffffffffffffffffffffffffffffffff", "success", "exited", "0")
	c, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err := b.WaitExit(c, "a"); err == nil {
		t.Fatal("WaitExit succeeded without the original receipt")
	}
	go func() { time.Sleep(50 * time.Millisecond); r.receipt(0, inv, "success", "exited", "0") }()
	rc, err := b.WaitExit(ctx, "a")
	if err != nil || !rc.Clean {
		t.Fatalf("%v %+v", err, rc)
	}
	h, _ := b.Inspect(ctx, "a")
	if h.State != vmlib.HostExited {
		t.Fatalf("%+v", h)
	}
}

func TestUncleanOrMismatchedReceipt(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "a")
	r.receipt(0, inv, "exit-code", "exited", "1")
	rc, err := b.WaitExit(ctx, "a")
	if err != nil || rc.Clean {
		t.Fatalf("%v %+v", err, rc)
	}
	if _, err := b.Publish(ctx, "a", "new"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("publish after unclean exit: %v", err)
	}
}

func TestPublishNoOverwriteAndBaseImmutable(t *testing.T) {
	r := newRig(t, 2)
	b := r.backend()
	baseSum := sum(t, filepath.Join(r.images, "base.ext4"))
	startRunning(t, r, b, "a")
	if _, err := b.Publish(ctx, "a", "golden"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("published a running VM: %v", err)
	}
	disk := filepath.Join(r.root, "vms", "a", "disk.ext4")
	os.WriteFile(disk, []byte("GUEST-WROTE-THIS"), 0o600)
	r.receipt(0, inv, "success", "exited", "0")
	img, err := b.Publish(ctx, "a", "golden")
	if err != nil || img.ID != "golden" || img.Base {
		t.Fatalf("%v %+v", err, img)
	}
	if got := sum(t, filepath.Join(r.images, "golden.ext4")); got != sum(t, disk) || img.SHA256 != got {
		t.Fatal("published image differs from the stopped disk")
	}
	if _, err := b.Publish(ctx, "a", "golden"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("overwrite: %v", err)
	}
	if _, err := b.Publish(ctx, "a", "base"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("base overwrite: %v", err)
	}
	if sum(t, filepath.Join(r.images, "base.ext4")) != baseSum {
		t.Fatal("base changed")
	}
	left, _ := filepath.Glob(filepath.Join(r.images, ".publish-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestReleaseOnlyWithProofAndFreesSlot(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "a")
	res, _ := b.Release(ctx, "a")
	if res.Released {
		t.Fatal("released a running VM's disk")
	}
	if _, err := prep(b, "b"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("retained VM's lease was freed: %v", err)
	}
	r.receipt(0, inv, "success", "exited", "0")
	if res, err := b.Release(ctx, "a"); err != nil || !res.Released {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(r.root, "vms", "a")); !os.IsNotExist(err) {
		t.Fatal("workspace not removed")
	}
	if res, err := b.Release(ctx, "a"); err != nil || !res.Released {
		t.Fatal("release must be idempotent")
	}
	if h, err := prep(b, "b"); err != nil || h.GuestIP != "172.17.0.2" {
		t.Fatalf("slot not reusable after release: %v %+v", err, h)
	}
}

func TestRestartReconciliationFailsClosed(t *testing.T) {
	r := newRig(t, 3)
	b := r.backend()
	startRunning(t, r, b, "run")  // slot 0, running, no receipt
	startRunning(t, r, b, "done") // slot 1, will have receipt
	prep(b, "idle")               // slot 2, never started
	r.receipt(1, inv, "success", "exited", "0")
	// done's invocation id equals run's in this simple fake; use a distinct id.
	b2 := r.backend() // "restart"
	h, _ := b2.Inspect(ctx, "run")
	if h.State != vmlib.HostRetained || !h.Retained {
		t.Fatalf("running VM without proof must be retained after restart: %+v", h)
	}
	if res, _ := b2.Release(ctx, "run"); res.Released {
		t.Fatal("freed disk on uncertain process state")
	}
	if h, _ := b2.Inspect(ctx, "idle"); h.State != vmlib.HostPrepared {
		t.Fatalf("%+v", h)
	}
	if _, err := prep(b2, "new"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("cap must count retained VMs: %v", err)
	}
}

func TestCorruptMetadataBlocksNewAdmission(t *testing.T) {
	r := newRig(t, 2)
	b := r.backend()
	prep(b, "a")
	os.WriteFile(filepath.Join(r.root, "vms", "a", "meta.json"), []byte("{broken"), 0o600)
	b2, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prep(b2, "b"); err == nil {
		t.Fatal("admitted a VM while host state is unreadable")
	}
	if _, err := os.Stat(filepath.Join(r.root, "vms", "a", "disk.ext4")); err != nil {
		t.Fatal("disk with unreadable metadata was removed")
	}
}

func TestFacadeServiceOverLocalBackendFlow(t *testing.T) {
	r := newRig(t, 3)
	b := r.backend()
	svc, _ := vmlib.NewService(b)
	vm, err := svc.Create(ctx, vmlib.CreateRequest{ID: "src", ImageID: "base", Profile: "small"})
	if err != nil || vm.State != vmlib.StateRunning {
		t.Fatalf("%v %+v", err, vm)
	}
	info, _ := svc.SSHInfo(ctx, "src")
	if info.Host != "172.17.0.2" || info.ProxyJump() != "jump@bastion.example.test" || info.AuthMode != vmlib.AuthImagePreinstalled {
		t.Fatalf("%+v", info)
	}
	go func() { time.Sleep(30 * time.Millisecond); r.receipt(0, inv, "success", "exited", "0") }()
	img, err := svc.Commit(ctx, "src", "golden")
	if err != nil || img.ID != "golden" {
		t.Fatalf("%v %+v", err, img)
	}
	for _, id := range []string{"c1", "c2"} {
		if _, err := svc.Create(ctx, vmlib.CreateRequest{ID: id, ImageID: "golden", Profile: "small"}); err != nil {
			t.Fatal(err)
		}
	}
}
