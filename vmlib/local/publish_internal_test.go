//go:build linux

package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

const testInv = "0123456789abcdef0123456789abcdef"

type pubRig struct {
	b           *Backend
	cfg         Config
	failPublish *atomic.Bool
}

func newPubRig(t *testing.T) *pubRig {
	t.Helper()
	d := t.TempDir()
	os.Chmod(d, 0o755)
	uid := uint32(os.Getuid())
	fail := &atomic.Bool{}
	cfg := Config{WorkRoot: filepath.Join(d, "w"), ImagesDir: filepath.Join(d, "i"), RunDir: filepath.Join(d, "r"), MaxVMs: 2,
		Profiles: []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 128, DiskMiB: 8}},
		Launcher: nopLauncher{}, VMM: nopVMM{}, MaxTotalMemMiB: 1024, MaxTotalVCPU: 8, UnitMemLimitMiB: 768, VMMOverheadMiB: 64,
		GuestPrefix: "172.30", TrustedOwnerUID: &uid, TrustedAncestorRoot: d,
		CopyFile: func(src, dst string) error {
			if strings.Contains(dst, ".publish-") && fail.Load() {
				return errors.New("publish copy failed")
			}
			return os.WriteFile(dst, []byte("disk:"+filepath.Base(filepath.Dir(src))), 0o600)
		},
	}
	for _, p := range []string{cfg.WorkRoot, cfg.ImagesDir, cfg.RunDir} {
		os.MkdirAll(p, 0o700)
	}
	os.WriteFile(filepath.Join(cfg.RunDir, ".backend.lock"), nil, 0o600)
	os.WriteFile(filepath.Join(cfg.ImagesDir, "base.ext4"), []byte("b"), 0o444)
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return &pubRig{b: b, cfg: cfg, failPublish: fail}
}

// exited starts vm and writes its clean receipt, so it can be published.
func (r *pubRig) exited(t *testing.T, id string, slot int) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.b.Prepare(ctx, vmlib.PrepareRequest{VMID: id, ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.b.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.cfg.RunDir, fmt.Sprintf("slot-%d", slot))
	os.MkdirAll(dir, 0o755)
	body := `{"invocation_id":"` + testInv + `","result":"success","exit_code":"exited","exit_status":"0"}`
	if err := os.WriteFile(filepath.Join(dir, testInv+".exit.json"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

// Finding 3: with two publishers for one target, a failing/aborting publisher
// must never delete (or reuse) the marker of an in-flight publisher.
func TestConcurrentPublishSameTargetIsReservedAndMarkerSurvives(t *testing.T) {
	r := newPubRig(t)
	ctx := context.Background()
	r.exited(t, "a", 0)
	r.exited(t, "b", 1)

	paused, gate := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	r.b.hookPublishAfterMarker = func(id string) {
		if armed.CompareAndSwap(true, false) {
			close(paused)
			<-gate
		}
	}
	aDone := make(chan error, 1)
	go func() { _, err := r.b.Publish(ctx, "a", "target"); aDone <- err }() // A: marker made, then suspended
	<-paused

	r.failPublish.Store(true) // B would fail its copy and roll back
	if _, err := r.b.Publish(ctx, "b", "target"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("second publisher of an in-flight target must be refused, got %v", err)
	}
	r.failPublish.Store(false)
	if _, err := os.Lstat(filepath.Join(r.cfg.ImagesDir, ".committed", "target")); err != nil {
		t.Fatal("B's refusal/rollback removed A's marker")
	}
	close(gate)
	select {
	case err := <-aDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("A never finished")
	}
	img, err := r.b.GetImage(ctx, "target")
	if err != nil || img.Base {
		t.Fatalf("published image misclassified as base: %v %+v", err, img)
	}
	if _, err := r.b.Publish(ctx, "b", "target"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("overwrite: %v", err)
	}
	// Classification and content survive a restart.
	r.b.Close()
	b2, err := New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	if g, err := b2.GetImage(ctx, "target"); err != nil || g.Base {
		t.Fatalf("%v %+v", err, g)
	}
	if data, _ := os.ReadFile(filepath.Join(r.cfg.ImagesDir, "target.ext4")); !strings.HasPrefix(string(data), "disk:a") {
		t.Fatalf("target holds the wrong VM's disk: %q", data)
	}
}

func TestFailedPublisherRemovesOnlyItsOwnArtifactsAndTargetIsReusable(t *testing.T) {
	r := newPubRig(t)
	ctx := context.Background()
	r.exited(t, "a", 0)
	r.exited(t, "b", 1)
	r.failPublish.Store(true)
	if _, err := r.b.Publish(ctx, "a", "target"); err == nil {
		t.Fatal("expected copy failure")
	}
	if _, err := os.Lstat(filepath.Join(r.cfg.ImagesDir, ".committed", "target")); !os.IsNotExist(err) {
		t.Fatal("rollback left a marker without an image")
	}
	if _, err := os.Lstat(filepath.Join(r.cfg.ImagesDir, "target.ext4")); !os.IsNotExist(err) {
		t.Fatal("image visible after a failed publish")
	}
	left, _ := filepath.Glob(filepath.Join(r.cfg.ImagesDir, ".publish-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	r.failPublish.Store(false)
	if _, err := r.b.Publish(ctx, "b", "target"); err != nil { // the reservation was released
		t.Fatal(err)
	}
	if g, _ := r.b.GetImage(ctx, "target"); g.Base {
		t.Fatalf("%+v", g)
	}
	if _, err := r.b.Publish(ctx, "a", "base"); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("base overwrite: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(r.cfg.ImagesDir, ".committed", "base")); err == nil {
		t.Fatal("a refused overwrite of the base created a committed marker for it")
	}
}
