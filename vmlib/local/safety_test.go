//go:build linux

package local_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

// ---- P1: exclusive ownership of the shared roots ---------------------------------

func TestSecondOwnerOfSameRootsIsRejectedAndCloseReleases(t *testing.T) {
	r := newRig(t, 1)
	b1 := r.backend()
	if _, err := local.New(r.cfg); err == nil {
		t.Fatal("a second Backend over the same roots was accepted (would double-allocate 172.17.0.2)")
	}
	base := filepath.Dir(r.root)
	cfg2 := r.cfg
	cfg2.WorkRoot = filepath.Join(base, "work2") // different workspace, SAME slot namespace (RunDir)
	os.MkdirAll(cfg2.WorkRoot, 0o700)
	if _, err := local.New(cfg2); err == nil {
		t.Fatal("same RunDir (slot/TAP/IP namespace) accepted by a second owner")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(r.root, alias); err != nil {
		t.Fatal(err)
	}
	cfg3 := r.cfg
	cfg3.WorkRoot = alias
	if _, err := local.New(cfg3); err == nil {
		t.Fatal("symlink alias of the workspace bypassed the ownership lock")
	}
	if _, err := prep(b1, "a"); err != nil {
		t.Fatal(err)
	}
	launches := len(r.launcher.slots)
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b1.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	if len(r.launcher.slots) != launches {
		t.Fatal("Close touched VMs")
	}
	if _, err := os.Stat(filepath.Join(r.root, "vms", "a", "disk.ext4")); err != nil {
		t.Fatal("Close removed a workspace")
	}
	b2 := r.backend() // reopen: reconciles the persisted lease
	if h, err := b2.Inspect(ctx, "a"); err != nil || h.State != vmlib.HostPrepared {
		t.Fatalf("%v %+v", err, h)
	}
	if _, err := prep(b2, "b"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("lease lost across close/reopen: %v", err)
	}
	// The lock files are never unlinked (no inode swap).
	if _, err := os.Stat(filepath.Join(r.run, ".backend.lock")); err != nil {
		t.Fatal(err)
	}
}

func TestOverlappingConfiguredRootsAreRejected(t *testing.T) {
	r := newRig(t, 1)
	cfg := r.cfg
	cfg.WorkRoot = filepath.Join(r.images, "w")
	os.MkdirAll(cfg.WorkRoot, 0o700)
	if b, err := local.New(cfg); err == nil {
		b.Close()
		t.Fatal("WorkRoot inside ImagesDir accepted")
	}
}

// ---- P1: restart / uncertain VMs can be stopped safely ----------------------------

func TestStopCommitDeleteAfterRestartUseIdentityCheckedShutdown(t *testing.T) {
	r := newRig(t, 2)
	b := r.backend()
	startRunning(t, r, b, "run")
	b.Close()
	b2 := r.backend()
	if h, _ := b2.Inspect(ctx, "run"); h.State != vmlib.HostRunning || !h.Retained {
		t.Fatalf("restart must keep the lifecycle state and flag retention: %+v", h)
	}
	r.vmm.onCtrl = func() { r.receipt(0, inv, "success", "exited", "0") }
	svc, _ := vmlib.NewService(b2)
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	vm, err := svc.Stop(c, "run")
	if err != nil || vm.State != vmlib.StateStopped {
		t.Fatalf("%v %+v", err, vm)
	}
	if r.vmm.ctrlAltDe != 1 {
		t.Fatalf("shutdown requests: %d", r.vmm.ctrlAltDe)
	}
	if img, err := svc.Commit(c, "run", "golden"); err != nil || img.ID != "golden" {
		t.Fatalf("%v %+v", err, img)
	}
	if res, err := svc.Delete(c, "run"); err != nil || !res.Deleted {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestRestartNeverSignalsAReplacementVMM(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "run")
	b.Close()
	b2 := r.backend()
	r.vmm.id = "fedcba9876543210fedcba9876543210"
	svc, _ := vmlib.NewService(b2)
	c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := svc.Stop(c, "run"); err == nil {
		t.Fatal("Stop succeeded against a replacement VMM")
	}
	if r.vmm.ctrlAltDe != 0 {
		t.Fatal("sent an action to a replacement VMM")
	}
}

func TestUncertainConfigurationIsStoppableByKnownInvocation(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	prep(b, "a")
	r.vmm.cfgErr = errors.New("socket reset")
	b.Start(ctx, "a")
	b.Close()
	b2 := r.backend()
	r.vmm.onCtrl = func() { r.receipt(0, inv, "success", "exited", "0") }
	svc, _ := vmlib.NewService(b2)
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if vm, err := svc.Stop(c, "a"); err != nil || vm.State != vmlib.StateStopped {
		t.Fatalf("%v %+v", err, vm)
	}
	if r.vmm.ctrlAltDe != 1 {
		t.Fatalf("ctrl-alt-del count %d", r.vmm.ctrlAltDe)
	}
}

func TestUnknownInvocationFailsClosedAcrossRestart(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	prep(b, "a")
	r.launcher.err = errors.New("helper lost its acknowledgement")
	if _, err := b.Start(ctx, "a"); err == nil {
		t.Fatal("expected launcher error")
	}
	b.Close()
	b2 := r.backend()
	svc, _ := vmlib.NewService(b2)
	c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := svc.Stop(c, "a"); err == nil {
		t.Fatal("Stop pretended to stop a VM with unknown identity")
	}
	if r.vmm.ctrlAltDe != 0 {
		t.Fatal("signalled a VMM of unknown identity")
	}
	if res, _ := b2.Release(ctx, "a"); res.Released {
		t.Fatal("discarded a disk that a helper-started unit may hold")
	}
	if _, err := os.Stat(filepath.Join(r.root, "vms", "a", "disk.ext4")); err != nil {
		t.Fatal("disk removed")
	}
}

// ---- P1: ambiguous launcher failure keeps the lease -------------------------------

func TestAmbiguousLauncherFailureKeepsLeaseDiskAndSlot(t *testing.T) {
	r := newRig(t, 2)
	b := r.backend()
	prep(b, "a")
	r.launcher.err = errors.New("lost ack")
	if _, err := b.Start(ctx, "a"); err == nil {
		t.Fatal("expected error")
	}
	h, _ := b.Inspect(ctx, "a")
	if !h.Retained || h.State == vmlib.HostPrepared {
		t.Fatalf("a possibly launched VM must not look like a plain prepared one: %+v", h)
	}
	if res, _ := b.Release(ctx, "a"); res.Released {
		t.Fatal("released after an ambiguous launch")
	}
	r.launcher.err = nil
	other, err := prep(b, "b")
	if err != nil || other.GuestIP == h.GuestIP {
		t.Fatalf("slot of an uncertain VM was reused: %v %+v", err, other)
	}
}

func TestLauncherIsNotCancelledByTheCallerAndStartCompletes(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	prep(b, "a")
	c, cancel := context.WithCancel(ctx)
	r.launcher.during = cancel // the caller's context ends while the helper is running
	h, err := b.Start(c, "a")
	if err != nil || h.State != vmlib.HostRunning || h.Retained {
		t.Fatalf("Start must finish once a unit may have launched: %v %+v", err, h)
	}
	if got := r.launcher.ctxErrAt; len(got) != 1 || got[0] != nil {
		t.Fatalf("helper invocation was cancelled with the caller (lost ack): %v", got)
	}
}

func TestPreCancelledStartLaunchesNothing(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	prep(b, "a")
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.Start(c, "a"); err == nil {
		t.Fatal("expected context error")
	}
	if len(r.launcher.slots) != 0 {
		t.Fatal("launched despite cancellation")
	}
	if h, _ := b.Inspect(ctx, "a"); h.State != vmlib.HostPrepared || h.Retained {
		t.Fatalf("%+v", h)
	}
}

// ---- P2: durability / atomic catalog ----------------------------------------------

func TestPublishFailsWithoutImageWhenClassificationCannotBeWritten(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "a")
	r.receipt(0, inv, "success", "exited", "0")
	if err := os.WriteFile(filepath.Join(r.images, ".committed"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(ctx, "a", "golden"); err == nil {
		t.Fatal("publish reported success although it could not classify the image")
	}
	if _, err := os.Lstat(filepath.Join(r.images, "golden.ext4")); !os.IsNotExist(err) {
		t.Fatal("image visible without its committed marker")
	}
}

func TestCommittedClassificationSurvivesRestart(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "a")
	r.receipt(0, inv, "success", "exited", "0")
	if _, err := b.Publish(ctx, "a", "golden"); err != nil {
		t.Fatal(err)
	}
	b.Close()
	b2 := r.backend()
	g, err := b2.GetImage(ctx, "golden")
	if err != nil || g.Base {
		t.Fatalf("%v %+v", err, g)
	}
	if base, _ := b2.GetImage(ctx, "base"); !base.Base {
		t.Fatal("base misclassified")
	}
}

// ---- admission: aggregate vCPU/memory --------------------------------------------

func TestAggregateMemoryAndVCPUAdmission(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.MaxTotalMemMiB = 512 // two 256 MiB guests
	b := r.backend()
	prep(b, "v1")
	prep(b, "v2")
	if _, err := prep(b, "v3"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("memory cap not enforced: %v", err)
	}
	c, _ := b.Capabilities(ctx)
	if c.MaxTotalMemMiB != 512 || c.MaxTotalVCPU != r.cfg.MaxTotalVCPU {
		t.Fatalf("%+v", c)
	}
	b.Release(ctx, "v2")
	if _, err := prep(b, "v3"); err != nil {
		t.Fatalf("freed capacity not reusable: %v", err)
	}
}

func TestAggregateVCPUAdmission(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.MaxTotalVCPU = 1
	b := r.backend()
	prep(b, "v1")
	if _, err := prep(b, "v2"); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("vcpu cap not enforced: %v", err)
	}
}

func TestProfilesCannotExceedInstalledUnitLimits(t *testing.T) {
	r := newRig(t, 1)
	r.cfg.Profiles = []vmlib.Profile{{Name: "big", VCPU: 1, MemMiB: 800, DiskMiB: 64}} // 800+64 > 768
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("profile larger than the per-unit memory limit accepted")
	}
	r = newRig(t, 1)
	r.cfg.MaxTotalMemMiB = 128 // smaller than one guest
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("profile larger than the aggregate budget accepted")
	}
}

func TestInspectReportsGuestNetworkMetadata(t *testing.T) {
	r := newRig(t, 2)
	r.cfg.GuestDNS = []string{"1.1.1.1"}
	b := r.backend()
	prep(b, "a")
	prep(b, "b")
	h, _ := b.Inspect(ctx, "b")
	g := h.Guest
	if g == nil || g.Address != "172.17.1.2/30" || g.Gateway != "172.17.1.1" || len(g.DNS) != 1 || g.Configuration != "mac-derived" {
		t.Fatalf("%+v", g)
	}
}

// ---- fail-closed storage mount guard -----------------------------------------------

func isMountpoint(p string) bool {
	var a, b syscall.Stat_t
	if syscall.Stat(p, &a) != nil || syscall.Stat(filepath.Dir(p), &b) != nil {
		return false
	}
	return a.Dev != b.Dev
}

func TestNonMountpointStorageFailsBeforeAnythingIsWritten(t *testing.T) {
	r := newRig(t, 1)
	r.cfg.StorageMount = t.TempDir() // an ordinary directory on the root filesystem
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("accepted a storage path that is not a mount point (root-fs fallback)")
	}
	for _, p := range []string{filepath.Join(r.root, "vms"), filepath.Join(r.root, ".backend.lock"), filepath.Join(r.run, ".backend.lock"), filepath.Join(r.images, ".backend.lock")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("wrote %s before validating the mount", p)
		}
	}
}

func TestRootsOnTheWrongDeviceAreRejected(t *testing.T) {
	if !isMountpoint("/dev/shm") {
		t.Skip("/dev/shm is not a mount point here")
	}
	r := newRig(t, 1) // roots live in t.TempDir(), not on /dev/shm
	r.cfg.StorageMount = "/dev/shm"
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatal("workspace on another device than the bounded storage mount accepted")
	}
}

func TestRootsOnTheStorageMountAreAccepted(t *testing.T) {
	if !isMountpoint("/dev/shm") {
		t.Skip("/dev/shm is not a mount point here")
	}
	r := newRig(t, 1)
	base, err := os.MkdirTemp("/dev/shm", "vmlib-test-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	r.cfg.WorkRoot, r.cfg.ImagesDir = filepath.Join(base, "work"), filepath.Join(base, "images")
	os.MkdirAll(r.cfg.WorkRoot, 0o700)
	os.MkdirAll(r.cfg.ImagesDir, 0o700)
	r.cfg.StorageMount = "/dev/shm"
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
}
