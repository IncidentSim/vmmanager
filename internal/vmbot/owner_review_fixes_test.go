package vmbot

// Regressions for the focused review of the sequential owner (see
// docs/vm-owner-verification.md, "Review fixes"). Each test names the review item.

import (
	"context"
	"errors"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// Item 1: the begin step writes "running"+transitional phase, then DBOS records
// the checkpoint. Losing that checkpoint re-runs begin for the same operation.
func TestReviewBeginIsIdempotentForCurrentOperation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"commit", "stop"} {
		e := newOwnerEnv(t)
		c := e.create("c-" + kind)
		e.ctl.engine().ready.Store(false) // admission only: nothing is delivered
		req := ControlRequest{Kind: kind, VMID: c.VMID, Name: "img-" + kind}
		op := e.accept("k-"+kind, req)
		cmd := OwnerCmd{OpID: op.ID, Kind: kind, Digest: op.Digest}
		for i := 0; i < 2; i++ { // the second call is the lost-checkpoint replay
			plan, err := e.ctl.beginOp(c.VMID, cmd)
			if err != nil || !plan.Run {
				t.Fatalf("%s begin #%d: run=%v note=%q err=%v", kind, i+1, plan.Run, plan.Note, err)
			}
		}
		if got := e.op(op.ID); got.Status != "running" {
			t.Fatalf("%s op %+v", kind, got)
		}
		if kind == "commit" {
			// Not permission to begin a stale commit: a merely pending op whose VM
			// already sits in the transitional phase is still rejected.
			_ = e.ctl.st.Update(func(s *State) {
				for n := range s.Operations {
					if s.Operations[n].ID == op.ID {
						s.Operations[n].Status = "pending"
					}
				}
			})
			if plan, _ := e.ctl.beginOp(c.VMID, cmd); plan.Run {
				t.Fatal("stale pending commit was begun")
			}
		}
		e.ctl.engine().ready.Store(true)
	}
}

// preparedLC reports a VM that exists only as a prepared workspace (a crash
// between backend Prepare and Start), and refuses nothing else.
type preparedLC struct {
	vmlib.Lifecycle
	prepared bool
}

func (p *preparedLC) Get(ctx context.Context, id string) (vmlib.VM, error) {
	if p.prepared {
		return vmlib.VM{ID: id, ImageID: "ubuntu", Profile: "small", State: vmlib.StatePreparing}, nil
	}
	return p.Lifecycle.Get(ctx, id)
}

// Item 2.
func TestReviewPreparedWorkspaceIsNotSuccessfulCreate(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	pl := &preparedLC{Lifecycle: e.lc.Lifecycle, prepared: true}
	e.lc.Lifecycle = pl
	o := e.accept("c1", ControlRequest{Kind: "create", ImageID: "ubuntu", Name: "keep"})
	got := awaitControl(t, e.ctl, ownerUser, o.ID)
	if got.Status == "succeeded" {
		t.Fatalf("prepared workspace reported as successful create: %+v", got)
	}
	v, err := e.ctl.ControlVM(ownerUser, o.VMID)
	if err != nil || v.Phase == PhaseActive || v.Name != "keep" || v.ImageID != "ubuntu" {
		t.Fatalf("record not retained as creating: %+v %v", v, err)
	}
	if n := e.lc.count("create:"); n != 0 {
		t.Fatalf("Create called over an existing workspace %d times", n)
	}
	// Occupancy is retained, and an explicit Delete is still safe.
	pl.prepared = false
	d := e.accept("d1", ControlRequest{Kind: "delete", VMID: o.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, d.ID); got.Status != "succeeded" {
		t.Fatalf("delete %+v", got)
	}
}

// Item 3: an earlier admitted command whose delivery was lost or delayed must be
// settled before a later Delete, in admission order.
func TestReviewDeleteSettlesEarlierUndeliveredCommands(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"stop", "commit"} {
		e := newOwnerEnv(t)
		c := e.create("c-" + kind)
		e.ctl.engine().ready.Store(false) // delivery of the earlier command fails
		first := e.accept("f-"+kind, ControlRequest{Kind: kind, VMID: c.VMID, Name: "golden"})
		if got := e.op(first.ID).Status; got != "pending" {
			t.Fatalf("%s should stay pending, is %s", kind, got)
		}
		e.ctl.engine().ready.Store(true)
		del := e.accept("d-"+kind, ControlRequest{Kind: "delete", VMID: c.VMID})
		if got := awaitControl(t, e.ctl, ownerUser, del.ID); got.Status != "succeeded" {
			t.Fatalf("delete %+v", got)
		}
		fo := e.op(first.ID)
		if fo.Status != "succeeded" {
			t.Fatalf("earlier %s left %q (%s) after owner success", kind, fo.Status, fo.ErrorCode)
		}
		if e.lc.count(kind+":") != 1 {
			t.Fatalf("%s calls %v", kind, e.lc.calls)
		}
		e.lc.mu.Lock()
		si, di := -1, -1
		for i, call := range e.lc.calls {
			if len(call) > len(kind) && call[:len(kind)+1] == kind+":" && si < 0 {
				si = i
			}
			if len(call) > 7 && call[:7] == "delete:" && di < 0 {
				di = i
			}
		}
		e.lc.mu.Unlock()
		if si < 0 || di < 0 || si > di {
			t.Fatalf("order %v", e.lc.calls)
		}
		for _, i := range e.st.Snapshot().Images {
			if i.Phase == "publishing" {
				t.Fatalf("dangling publishing image %+v", i)
			}
		}
	}
}

// Item 4.
func TestReviewSparseLegacyRecordGetsProjection(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	e.close()
	svc, _ := vmlib.NewService(vmlibtest.New(4, "ubuntu"))
	e.svc, e.lc = svc, newGateLC(svc)
	const id = "bot-0123456789abcdef"
	if _, err := svc.Create(context.Background(), vmlib.CreateRequest{ID: id, ImageID: "ubuntu", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	st, err := OpenControlStore(e.dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := randToken()
	if err := st.Update(func(s *State) {
		s.Generation = 1
		s.VMs = append(s.VMs, VMRecord{ID: id, Token: tok, Phase: PhaseActive, Generation: 1, Label: "old", OwnerUserID: ownerUser})
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	e.open(true)
	e.eventually("projection populated", func() bool {
		v, err := e.ctl.ControlVM(ownerUser, id)
		return err == nil && v.ImageID == "ubuntu" && v.Resources != nil && v.State != ""
	})
	r, _ := e.st.Snapshot().ByID(id)
	if r.Token != tok || r.Label != "old" || r.OwnerUserID != ownerUser || r.Phase != PhaseActive {
		t.Fatalf("identity changed: %+v", r)
	}
	if e.lc.count("create:") != 0 {
		t.Fatal("bootstrap called Create")
	}
}

// Item 5.
func TestReviewTerminalOnStoppedIsImmediateConflict(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	s := e.accept("s1", ControlRequest{Kind: "stop", VMID: c.VMID})
	awaitControl(t, e.ctl, ownerUser, s.ID)
	_, err := e.ctl.AcceptControl(ownerUser, "t1", ControlRequest{Kind: "terminal", VMID: c.VMID})
	if !errors.Is(err, ErrControlConflict) {
		t.Fatalf("terminal on stopped: %v", err)
	}
}

// Independent review: actual late Send while Delete's effect is still blocked.
func TestReviewLateSendWhileDeleteBlocked(t *testing.T) {
	for _, kind := range []string{"stop", "commit"} {
		t.Run(kind, func(t *testing.T) {
			e := newOwnerEnv(t)
			created := e.create("create")
			e.ctl.engine().ready.Store(false)
			first := e.accept("first", ControlRequest{Kind: kind, VMID: created.VMID, Name: "late-image"})
			e.ctl.engine().ready.Store(true)
			release := e.lc.arm("delete")
			defer release()
			deleted := e.accept("delete", ControlRequest{Kind: "delete", VMID: created.VMID})
			e.lc.waitEntered(t, "delete")
			if got := e.op(first.ID); got.Status != "succeeded" {
				t.Fatalf("prior not settled: %+v", got)
			}
			before := e.lc.count(kind + ":")
			if err := e.ctl.deliver(first); err != nil {
				t.Fatalf("actual delayed Send: %v", err)
			}
			release()
			if got := awaitControl(t, e.ctl, ownerUser, deleted.ID); got.Status != "succeeded" {
				t.Fatalf("delete: %+v", got)
			}
			e.eventually("owner success", func() bool { return string(e.ownerStatus(created.VMID)) == "SUCCESS" })
			if got := e.lc.count(kind + ":"); got != before {
				t.Fatalf("late command reran: %d -> %d", before, got)
			}
			e.close()
			e.open(true)
			if got := e.lc.count(kind + ":"); got != before {
				t.Fatalf("late command reran after recovery: %d -> %d", before, got)
			}
			if got := e.op(first.ID); got.Status != "succeeded" {
				t.Fatalf("prior outcome changed: %+v", got)
			}
		})
	}
}
