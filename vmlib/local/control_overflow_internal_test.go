//go:build linux

package local

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"testing"
)

func TestReservationSumDoesNotWrap(t *testing.T) {
	r := newPubRig(t)
	// A restart may retain reservations larger than today's policy. Sum in a
	// wider type so metadata cannot wrap the aggregate and re-open admission.
	r.b.entries["old-a"] = &entry{m: meta{ID: "old-a", ImageID: "base", Slot: 0, MemMiB: 1 << 31, VCPU: 1 << 31}}
	r.b.entries["old-b"] = &entry{m: meta{ID: "old-b", ImageID: "base", Slot: 1, MemMiB: 1 << 31, VCPU: 1 << 31}}
	r.b.cfg.MaxVMs = 3
	if _, err := r.b.Prepare(context.Background(), vmlib.PrepareRequest{VMID: "new", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatalf("overflow reopened admission: %v", err)
	}
}

func TestProfileOverheadCannotWrapUnitAuthority(t *testing.T) {
	r := newPubRig(t)
	r.b.Close()
	cfg := r.cfg
	cfg.Profiles = []vmlib.Profile{{Name: "huge", VCPU: 1, MemMiB: ^uint32(0), DiskMiB: 8}}
	cfg.MaxTotalMemMiB = ^uint32(0)
	b, err := New(cfg)
	if err == nil {
		b.Close()
		t.Fatal("memory+overhead overflow bypassed installed unit authority")
	}
}
