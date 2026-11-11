package vmbot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func TestAdminBatchExpiryCloneAndRestart(t *testing.T) {
	e := newOwnerEnv(t)
	e.create("seed")
	if err := e.ctl.ConfigureAdmins([]int64{99}); err != nil {
		t.Fatal(err)
	}
	b, err := e.ctl.AdminPreview(99, 99, "preview")
	if err != nil {
		t.Fatal(err)
	}
	b.VMs[0].ID = "tampered-return"
	snap := e.st.Snapshot()
	original := snap.AdminBatches[0].VMs[0].ID
	if original == "tampered-return" {
		t.Fatal("returned preview aliases state")
	}
	snap.AdminBatches[0].VMs[0].ID = "tampered-snapshot"
	if e.st.Snapshot().AdminBatches[0].VMs[0].ID != original {
		t.Fatal("snapshot aliases state")
	}
	e.ctl.clock = func() time.Time { return b.ExpiresAt }
	if _, err = e.ctl.AdminAdvance(99, 99, b.Token); !errors.Is(err, ErrControlConflict) {
		t.Fatalf("expired confirm: %v", err)
	}
	if _, err = e.ctl.AdminAdvance(99, 100, b.Token); !errors.Is(err, ErrControlForbidden) {
		t.Fatalf("foreign chat: %v", err)
	}
	if _, err = e.ctl.AdminAdvance(99, 99, strings.Repeat("x", 43)); !errors.Is(err, ErrNoVM) {
		t.Fatalf("stale token: %v", err)
	}
	e.ctl.clock = nil
	e.reopen(false)
	e.ctl.ConfigureAdmins([]int64{99})
	repeat, err := e.ctl.AdminPreview(99, 99, "preview")
	if err != nil || repeat.Token != b.Token || repeat.VMs[0].ID != original {
		t.Fatalf("restart broadened snapshot: %+v %v", repeat, err)
	}
	b, err = e.ctl.AdminAdvance(99, 99, b.Token)
	if err != nil {
		t.Fatal(err)
	}
	// Confirmed transactions continue past the preview expiry; restart must
	// not turn them into a fresh snapshot or a second per-ID operation.
	e.reopen(false)
	e.ctl.ConfigureAdmins([]int64{99})
	e.ctl.clock = func() time.Time { return b.ExpiresAt.Add(time.Hour) }
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err = e.ctl.AdminAdvance(99, 99, b.Token)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending == 0 {
			break
		}
		time.Sleep(time.Millisecond * 5)
	}
	if b.Completed != 1 || b.Pending != 0 {
		t.Fatalf("restart result: %+v", b)
	}
	count := 0
	for _, op := range e.st.Snapshot().Operations {
		if op.Kind == "delete" {
			count++
			if op.AdminUserID != 99 || op.OwnerUserID != ownerUser {
				t.Fatalf("audit/owner: %+v", op)
			}
		}
	}
	if count != 1 {
		t.Fatalf("repeated delete operations: %d", count)
	}
}

func TestAdminBatchRetainedFailureUsesSafeOwnerPath(t *testing.T) {
	var deletes int
	e := startCleanupEnv(t, func(string) error { return nil }, func(context.Context, string) (vmlib.DeleteResult, error) {
		deletes++
		return vmlib.DeleteResult{}, errors.New("uncertain stop; retain workspace")
	})
	created := e.create("seed")
	e.ctl.ConfigureAdmins([]int64{99})
	// Revoking the original user's ordinary access must not make the inventory
	// incomplete or let an administrator select an arbitrary acting owner.
	e.ctl.ConfigureControl(ControlPolicy{AllowedUsers: []int64{22}, SharedImages: []string{"ubuntu"}, MaxVMs: 3, MaxVMsPerUser: 3})
	b, err := e.ctl.AdminPreview(99, 99, "preview")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err = e.ctl.AdminAdvance(99, 99, b.Token)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending == 0 {
			break
		}
		time.Sleep(time.Millisecond * 5)
	}
	if b.Failed != 1 || b.Completed != 0 || b.Pending != 0 {
		t.Fatalf("false success: %+v", b)
	}
	if _, ok := e.record(created.VMID); !ok {
		t.Fatal("uncertain VM removed")
	}
	before := deletes
	if _, err = e.ctl.AdminAdvance(99, 99, b.Token); err != nil {
		t.Fatal(err)
	}
	if deletes != before || deletes < 1 {
		t.Fatalf("replay retried failed delete: before %d after %d", before, deletes)
	}
}

func TestAdminBatchStateValidation(t *testing.T) {
	e := newOwnerEnv(t)
	e.create("seed")
	e.ctl.ConfigureAdmins([]int64{99})
	b, err := e.ctl.AdminPreview(99, 99, "preview")
	if err != nil {
		t.Fatal(err)
	}
	if err = e.st.Update(func(s *State) { s.AdminBatches[0].VMs = append(s.AdminBatches[0].VMs, s.AdminBatches[0].VMs[0]) }); err == nil {
		t.Fatal("accepted duplicate snapshot ID")
	}
	if err = e.st.Update(func(s *State) { s.AdminBatches[0].Token = "not-opaque" }); err == nil {
		t.Fatal("accepted malformed token")
	}
	if err = e.st.Update(func(s *State) { s.AdminBatches[0].VMs[0].ID = "replacement" }); err == nil {
		t.Fatal("accepted rewritten immutable snapshot")
	}
	if got := e.st.Snapshot().AdminBatches[0]; got.Token != b.Token {
		t.Fatal("failed write changed batch")
	}
}
