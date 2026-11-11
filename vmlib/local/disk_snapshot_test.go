//go:build linux

package local_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type diskTerms struct{}

func (*diskTerms) Start(context.Context, vmbot.TerminalTarget) error { return nil }
func (*diskTerms) Revoke(string)                                     {}
func (*diskTerms) Close()                                            {}

// Actual owner -> facade -> local Prepare admission, sparse storage/copy seam,
// fake launcher/VMM only. No root, real guests or multi-gigabyte allocation.
func TestOwnerSelectedDiskSnapshotSurvivesRestart(t *testing.T) {
	r := newRig(t, 3)
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, CPULimitMilli: 500, MemMiB: 128, DiskMiB: 4096}, {Name: "large", VCPU: 2, CPULimitMilli: 1000, MemMiB: 256, DiskMiB: 8192}}
	image := filepath.Join(r.images, "base.ext4")
	if err := os.Chmod(image, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(image, 6000<<20); err != nil {
		t.Fatal(err)
	}
	r.cfg.CopyFile = func(src, dst string) error {
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Truncate(info.Size())
	}
	b := r.backend()
	stateDir := filepath.Join(t.TempDir(), "state")
	st, err := vmbot.OpenControlStore(stateDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	ctl := vmbot.NewController(svc, st, &diskTerms{}, vmbot.ControllerConfig{Profile: "small", OpTimeout: 5 * time.Second})
	ctl.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"base"}, MaxVMs: 3, MaxVMsPerUser: 3})
	if err := ctl.Start(ctx); err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			ctl.Close()
			st.Close()
		}
	}()
	req := vmbot.ControlRequest{Kind: "create", ImageID: "base", Profile: "large"}
	op, err := ctl.AcceptControl(11, "large", req)
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(8 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got, e := ctl.ControlOperation(11, op.ID)
		if e != nil {
			t.Fatal(e)
		}
		if got.Status == "succeeded" {
			break
		}
		if got.Status == "failed" || time.Now().After(end) {
			t.Fatalf("large image admission %+v", got)
		}
	}
	record, ok := st.Snapshot().ByID(op.VMID)
	if !ok || record.DiskMiB != 8192 || record.Profile != "large" {
		t.Fatalf("admission disk snapshot %+v", record)
	}
	checkMeta := func() {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(r.root, "vms", op.VMID, "meta.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			DiskMiB uint32 `json:"disk_mib"`
		}
		if err := json.Unmarshal(raw, &m); err != nil || m.DiskMiB != 8192 {
			t.Fatalf("backend disk reservation %s %v", raw, err)
		}
	}
	checkMeta()
	ctl.Close()
	st.Close()
	closed = true
	b.Close()
	// Remove large, lower the remaining CPU/RAM/disk catalogue. Existing and
	// deferred admitted snapshots must not be reinterpreted through that catalogue.
	r.cfg.Profiles = []vmlib.Profile{{Name: "small", VCPU: 1, CPULimitMilli: 500, MemMiB: 128, DiskMiB: 4096}}
	b = r.backend()
	checkMeta()
	st, err = vmbot.OpenControlStore(stateDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err = vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	ctl = vmbot.NewController(svc, st, &diskTerms{}, vmbot.ControllerConfig{Profile: "small"})
	ctl.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"base"}, MaxVMs: 3, MaxVMsPerUser: 3})
	// Settled replay does not require an executor or catalogue lookup.
	again, err := ctl.AcceptControl(11, "large", req)
	if err != nil || again.ID != op.ID {
		t.Fatalf("restart replay %+v %v", again, err)
	}
	restored, _ := st.Snapshot().ByID(op.VMID)
	if restored.DiskMiB != 8192 {
		t.Fatal("restart changed disk bound")
	}
	resources := &vmlib.VMResources{VCPU: record.Resources.VCPU, MemMiB: record.Resources.MemoryMiB, CPULimitMilli: record.Resources.CPULimitMilli}
	prep := vmlib.PrepareRequest{VMID: op.VMID, ImageID: "base", Resources: resources, DiskMiB: record.DiskMiB}
	if _, err := b.Prepare(ctx, prep); err != nil {
		t.Fatalf("existing reservation replay: %v", err)
	}
	prep.DiskMiB = 4096
	if _, err := b.Prepare(ctx, prep); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("changed snapshot aliases: %v", err)
	}
	prep.VMID = "deferred"
	prep.DiskMiB = record.DiskMiB
	if _, err := b.Prepare(ctx, prep); err != nil {
		t.Fatalf("deferred original snapshot after removal: %v", err)
	}
	prep.VMID = "new-small"
	prep.Resources = nil
	prep.DiskMiB = 0
	prep.Profile = "small"
	if _, err := b.Prepare(ctx, prep); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatalf("small accepted oversized image: %v", err)
	}
	// Historical explicit CPU/RAM without a disk snapshot retains smallest-bound
	// semantics, rather than silently acquiring large-profile disk permission.
	prep.Profile = ""
	prep.Resources = &vmlib.VMResources{VCPU: 1, MemMiB: 128, CPULimitMilli: 500}
	if _, err := b.Prepare(ctx, prep); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatalf("legacy disk migration changed: %v", err)
	}
}
