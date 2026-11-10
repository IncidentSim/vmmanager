//go:build linux

package local

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPreparePublicationReservationIsDefinitiveAdmissionRefusal(t *testing.T) {
	r := newPubRig(t)
	// Models the short interval after a publisher made its image visible but
	// before its target reservation is released. No VM was admitted on refusal.
	r.b.publishing["base"] = struct{}{}
	if _, err := r.b.Prepare(context.Background(), vmlib.PrepareRequest{VMID: "new", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("publication reservation must be a definitive admission error: %v", err)
	}
	delete(r.b.publishing, "base")
	if _, err := r.b.Prepare(context.Background(), vmlib.PrepareRequest{VMID: "new", ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteImageReferencesRestartAndBase(t *testing.T) {
	r := newPubRig(t)
	ctx := context.Background()
	r.exited(t, "source", 0)
	if _, err := r.b.Publish(ctx, "source", "derived"); err != nil {
		t.Fatal(err)
	}
	if err := r.b.DeleteImage(ctx, "base"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("base: %v", err)
	}
	if _, err := r.b.Prepare(ctx, vmlib.PrepareRequest{VMID: "consumer", ImageID: "derived", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	if err := r.b.DeleteImage(ctx, "derived"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("reference: %v", err)
	}
	r.b.Close()
	b, err := New(r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.DeleteImage(ctx, "derived"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("restart reference: %v", err)
	}
	if _, err := b.Release(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteImage(ctx, "derived"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteImage(ctx, "derived"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetImage(ctx, "derived"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatal(err)
	}
	if err := b.DeleteImage(ctx, "../base"); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatal(err)
	}
}

func TestDeleteImageRefusesInFlightPublication(t *testing.T) {
	r := newPubRig(t)
	ctx := context.Background()
	r.exited(t, "source", 0)
	paused, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(gate) })
	r.b.hookPublishAfterMarker = func(string) { close(paused); <-gate }
	done := make(chan error, 1)
	go func() { _, err := r.b.Publish(ctx, "source", "target"); done <- err }()
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("publisher not paused")
	}
	if err := r.b.DeleteImage(ctx, "target"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("publication: %v", err)
	}
	once.Do(func() { close(gate) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := r.b.DeleteImage(ctx, "target"); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteImageRefusesPreparingCopy(t *testing.T) {
	r := newPubRig(t)
	ctx := context.Background()
	r.exited(t, "source", 0)
	if _, err := r.b.Publish(ctx, "source", "derived"); err != nil {
		t.Fatal(err)
	}
	paused, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(gate) })
	original := r.b.cfg.CopyFile
	r.b.cfg.CopyFile = func(src, dst string) error { close(paused); <-gate; return original(src, dst) }
	done := make(chan error, 1)
	go func() {
		_, err := r.b.Prepare(ctx, vmlib.PrepareRequest{VMID: "consumer", ImageID: "derived", Profile: "small"})
		done <- err
	}()
	select {
	case <-paused:
	case <-time.After(3 * time.Second):
		t.Fatal("prepare not paused")
	}
	if err := r.b.DeleteImage(ctx, "derived"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("preparing image deletion: %v", err)
	}
	once.Do(func() { close(gate) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := r.b.Release(ctx, "consumer"); err != nil {
		t.Fatal(err)
	}
	if err := r.b.DeleteImage(ctx, "derived"); err != nil {
		t.Fatal(err)
	}
}

type admissionPauseContext struct {
	context.Context
	paused, gate chan struct{}
	once         sync.Once
}

func (c *admissionPauseContext) Err() error {
	c.once.Do(func() { close(c.paused); <-c.gate })
	return nil
}

func TestDeleteImageWinsBeforePrepareAdmission(t *testing.T) {
	r := newPubRig(t)
	ctx := context.Background()
	r.exited(t, "source", 0)
	if _, err := r.b.Publish(ctx, "source", "derived"); err != nil {
		t.Fatal(err)
	}
	c := &admissionPauseContext{Context: ctx, paused: make(chan struct{}), gate: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(c.gate) })
	done := make(chan error, 1)
	go func() {
		_, err := r.b.Prepare(c, vmlib.PrepareRequest{VMID: "consumer", ImageID: "derived", Profile: "small"})
		done <- err
	}()
	select {
	case <-c.paused:
	case <-time.After(3 * time.Second):
		t.Fatal("prepare not paused")
	}
	if err := r.b.DeleteImage(ctx, "derived"); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(c.gate) })
	if err := <-done; !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("deleted image admitted: %v", err)
	}
}

func TestDeleteImageRejectsSymlinkCatalog(t *testing.T) {
	r := newPubRig(t)
	ctx := context.Background()
	r.exited(t, "source", 0)
	if _, err := r.b.Publish(ctx, "source", "target"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep")
	os.WriteFile(outside, []byte("safe"), 0600)
	target := filepath.Join(r.cfg.ImagesDir, "target.ext4")
	os.Remove(target)
	os.Symlink(outside, target)
	if err := r.b.DeleteImage(ctx, "target"); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatalf("symlink: %v", err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "safe" {
		t.Fatal("outside modified")
	}
}
