package vmbot

// Durable TTL tests (docs/plans/vm-policy-access.md stream C). Real DBOS over
// file SQLite, short real deadlines, the in-memory vmlib fake. They prove the
// owner/store/replay behavior; they say nothing about live Firecracker.
//
// Run focused: GOMAXPROCS=2 go test ./internal/vmbot -run 'TTL|DBOSSDK' -count=1

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// reopen closes the controller+store and opens them again on the same directory.
// legacy=true registers the frozen baseline (5b3ac16) owner body instead of vmOwner.
func (e *ownerEnv) reopen(legacy bool) {
	e.t.Helper()
	e.close()
	var err error
	if e.st, err = OpenControlStore(e.dir, 0); err != nil {
		e.t.Fatal(err)
	}
	e.ctl = NewController(e.lc, e.st, e.terms, ControllerConfig{Profile: "small", PublicOrigin: "https://t.example.test", OpTimeout: 3 * time.Second, QueryTimeout: time.Second})
	e.ctl.ConfigureControl(ControlPolicy{AllowedUsers: []int64{ownerUser, 22}, SharedImages: []string{"ubuntu"}, MaxVMs: 3, MaxVMsPerUser: 3})
	if legacy {
		e.ctl.ownerFn = legacyBaselineOwner
	}
	if err = e.ctl.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	if err = e.ctl.RecoverControl(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *ownerEnv) record(vm string) (VMRecord, bool) { return e.st.Snapshot().ByID(vm) }

func (e *ownerEnv) gone(vm string) bool {
	_, ok := e.record(vm)
	return !ok
}

func (e *ownerEnv) stepNames(vm string) []string {
	e.t.Helper()
	steps, err := dbos.GetWorkflowSteps(e.ctl.engine().ctx, ownerWorkflowID(vm))
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, s := range steps {
		out = append(out, s.StepName)
	}
	return out
}

func hasStep(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func (e *ownerEnv) createTTL(key string, ttl int64) ControlOperation {
	e.t.Helper()
	o := e.accept(key, ControlRequest{Kind: "create", ImageID: "ubuntu", TTLSeconds: ttl})
	if got := awaitControl(e.t, e.ctl, ownerUser, o.ID); got.Status != "succeeded" {
		e.t.Fatalf("create %+v", got)
	}
	return o
}

// legacyBaselineOwner is a frozen copy of vmOwner at the approved baseline
// 5b3ac16 (before dbos.Patch / expiry). It stands in for an owner workflow whose
// history was written by the previous release.
func legacyBaselineOwner(ctx dbos.Context, in OwnerIn) (OwnerOut, error) {
	c := controllerOf(ctx)
	if in.CreateOpID != "" {
		if _, err := c.ownerHandle(ctx, in.VMID, OwnerCmd{OpID: in.CreateOpID, Kind: "create", Digest: in.CreateDigest}); err != nil {
			return OwnerOut{}, err
		}
	}
	for {
		c.rebuildTerminal(in.VMID)
		cmd, err := dbos.Recv[OwnerCmd](ctx, ownerCmdTopic, ownerIdle)
		if err != nil {
			var de *dbos.Error
			if errors.As(err, &de) && de.Code == dbos.ErrorCodeTimeout {
				continue
			}
			return OwnerOut{}, err
		}
		if cmd.Kind == "delete" {
			prior, err := ownerStep(ctx, "prior", 3, func(context.Context) ([]OwnerCmd, error) { return c.priorUnfinished(in.VMID, cmd.OpID), nil })
			if err != nil {
				return OwnerOut{}, err
			}
			for _, p := range prior {
				if _, err := c.ownerHandle(ctx, in.VMID, p); err != nil {
					return OwnerOut{}, err
				}
			}
		}
		gone, err := c.ownerHandle(ctx, in.VMID, cmd)
		if err != nil {
			return OwnerOut{}, err
		}
		if gone {
			return OwnerOut{VMID: in.VMID, Phase: "deleted"}, nil
		}
	}
}

func TestTTLDefaultsBoundsAndPersistence(t *testing.T) {
	e := newOwnerEnv(t)
	before := time.Now()
	def := e.create("c-default")
	v, err := e.ctl.ControlVM(ownerUser, def.VMID)
	if err != nil {
		t.Fatal(err)
	}
	if v.TTLSeconds != 3600 || v.ExpiresAt == nil {
		t.Fatalf("default ttl view %+v", v)
	}
	if d := v.ExpiresAt.Sub(before); d < 3599*time.Second || d > 3601*time.Second {
		t.Fatalf("expires_at %v not ~now+3600s", v.ExpiresAt)
	}
	max := e.createTTL("c-max", 86400)
	if mv, _ := e.ctl.ControlVM(ownerUser, max.VMID); mv.TTLSeconds != 86400 {
		t.Fatalf("max ttl %+v", mv)
	}
	for _, bad := range []int64{86401, -1, 1 << 40, 1<<63 - 1, 18446744074} {
		if _, err := e.ctl.AcceptControl(ownerUser, "bad", ControlRequest{Kind: "create", ImageID: "ubuntu", TTLSeconds: bad}); !errors.Is(err, vmlib.ErrInvalidArgument) {
			t.Fatalf("ttl %d: %v", bad, err)
		}
	}
	if _, err := e.ctl.AcceptControl(ownerUser, "stop-ttl", ControlRequest{Kind: "stop", VMID: def.VMID, TTLSeconds: 5}); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatalf("ttl on non-create must be rejected: %v", err)
	}
	// Idempotent replay returns the recorded operation and never moves the deadline.
	again, err := e.ctl.AcceptControl(ownerUser, "c-default", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if err != nil || again.ID != def.ID || again.VMID != def.VMID {
		t.Fatalf("replay %+v %v", again, err)
	}
	// A different effective request under the same key is a conflict.
	if _, err = e.ctl.AcceptControl(ownerUser, "c-default", ControlRequest{Kind: "create", ImageID: "ubuntu", TTLSeconds: 60}); !errors.Is(err, ErrControlConflict) {
		t.Fatalf("digest conflict: %v", err)
	}
	want := *v.ExpiresAt
	e.reopen(false) // restart must not reset the deadline
	r, ok := e.record(def.VMID)
	if !ok || r.ExpiresAt == nil || !r.ExpiresAt.Equal(want) || r.TTLSeconds != 3600 {
		t.Fatalf("persisted deadline changed: %+v want %v", r, want)
	}
	if v2, _ := e.ctl.ControlVM(ownerUser, def.VMID); v2.ExpiresAt == nil || !v2.ExpiresAt.Equal(want) {
		t.Fatalf("API view after restart: %+v", v2)
	}
}

func TestTTLExpiryRunsSafeDeleteThroughOwner(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.createTTL("c1", 1)
	deadline := *mustRecord(t, e, c.VMID).ExpiresAt
	e.eventually("expired VM deleted by its owner", func() bool { return e.gone(c.VMID) })
	if time.Now().Before(deadline) {
		t.Fatalf("deleted before deadline %v", deadline)
	}
	if _, err := e.svc.Get(context.Background(), c.VMID); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("backend VM must be deleted via Delete: %v", err)
	}
	if n := e.lc.count("delete:"); n != 1 {
		t.Fatalf("backend delete calls = %d", n)
	}
	e.eventually("owner ends", func() bool { return e.ownerStatus(c.VMID) == dbos.WorkflowStatusSuccess })
	var ttlOps []ControlOperation
	for _, o := range e.st.Snapshot().Operations {
		if o.VMID == c.VMID && o.Kind == "delete" {
			ttlOps = append(ttlOps, o)
		}
	}
	if len(ttlOps) != 1 || ttlOps[0].Status != "succeeded" || ttlOps[0].Exec != execOwner || ttlOps[0].OwnerUserID != ownerUser {
		t.Fatalf("expiry delete op: %+v", ttlOps)
	}
	if !hasStep(e.stepNames(c.VMID), "DBOS.patch-"+ownerTTLPatch) {
		t.Fatalf("new owner must record the patch marker: %v", e.stepNames(c.VMID))
	}
}

func mustRecord(t *testing.T, e *ownerEnv, vm string) VMRecord {
	t.Helper()
	r, ok := e.record(vm)
	if !ok || r.ExpiresAt == nil {
		t.Fatalf("no deadline record for %s: %+v", vm, r)
	}
	return r
}

// The deadline never preempts: a busy child finishes first, nothing is killed or
// freed, new ordinary commands are refused with vm_expired, and the owner then
// deletes once.
func TestTTLBusyChildNotPreemptedAndCommandsRefused(t *testing.T) {
	e := newOwnerEnv(t)
	// Leave enough admission/SQLite scheduling headroom under the full race
	// suite; keep the child bound beyond the real deadline being exercised.
	e.ctl.cfg.OpTimeout = 10 * time.Second
	c := e.createTTL("c1", 3)
	release := e.lc.arm("stop")
	defer release()
	stop := e.accept("s1", ControlRequest{Kind: "stop", VMID: c.VMID})
	e.lc.waitEntered(t, "stop")
	deadline := *mustRecord(t, e, c.VMID).ExpiresAt
	time.Sleep(time.Until(deadline) + 400*time.Millisecond)

	if n := e.lc.count("delete:"); n != 0 {
		t.Fatalf("delete ran %d times while the child was busy", n)
	}
	if _, err := e.svc.Get(context.Background(), c.VMID); err != nil {
		t.Fatalf("VM freed/killed on timeout: %v", err)
	}
	// ordinary commands: vm_expired; auth and ownership are decided first.
	for _, kind := range []string{"stop", "commit", "terminal"} {
		_, err := e.ctl.AcceptControl(ownerUser, "late-"+kind, ControlRequest{Kind: kind, VMID: c.VMID, Name: "img"})
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("%s after expiry: %v", kind, err)
		}
	}
	if _, err := e.ctl.AcceptControl(22, "foreign", ControlRequest{Kind: "stop", VMID: c.VMID}); !errors.Is(err, ErrNoVM) {
		t.Fatalf("foreign owner must see no VM, got %v", err)
	}
	if _, err := e.ctl.AcceptControl(99, "stranger", ControlRequest{Kind: "stop", VMID: c.VMID}); !errors.Is(err, ErrControlForbidden) {
		t.Fatalf("unauthorized must be forbidden, got %v", err)
	}
	// Idempotency: the already-accepted stop replays as recorded; a mutated body conflicts.
	if rep, err := e.ctl.AcceptControl(ownerUser, "s1", ControlRequest{Kind: "stop", VMID: c.VMID}); err != nil || rep.ID != stop.ID {
		t.Fatalf("replay after expiry: %+v %v", rep, err)
	}
	if _, err := e.ctl.AcceptControl(ownerUser, "s1", ControlRequest{Kind: "commit", VMID: c.VMID, Name: "x"}); !errors.Is(err, ErrControlConflict) {
		t.Fatalf("mutated replay: %v", err)
	}
	// Rejected requests did not record operations.
	for _, o := range e.st.Snapshot().Operations {
		if strings.HasPrefix(o.Key, "late-") {
			t.Fatalf("expired rejection recorded an operation: %+v", o)
		}
	}
	if got := e.ownerStatus(c.VMID); got != dbos.WorkflowStatusPending {
		t.Fatalf("owner %s", got)
	}
	release()
	if got := awaitControl(t, e.ctl, ownerUser, stop.ID); got.Status != "succeeded" {
		t.Fatalf("stop must finish normally: %+v", got)
	}
	e.eventually("expiry delete after the child", func() bool { return e.gone(c.VMID) })
	e.lc.mu.Lock()
	defer e.lc.mu.Unlock()
	si, di := -1, -1
	for i, call := range e.lc.calls {
		if strings.HasPrefix(call, "stop:exit") || call == "stop:"+c.VMID {
			si = i
		}
		if strings.HasPrefix(call, "delete:") && di < 0 {
			di = i
		}
	}
	if di < 0 || (si >= 0 && si > di) {
		t.Fatalf("delete must follow the stop child: %v", e.lc.calls)
	}
}

// A restart must neither reset nor lose the deadline: the owner is recovered
// mid-Recv and still deletes at the ORIGINAL expires_at.
func TestTTLRestartDoesNotResetDeadline(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.createTTL("c1", 3)
	rec := mustRecord(t, e, c.VMID)
	deadline := *rec.ExpiresAt
	time.Sleep(500 * time.Millisecond)
	e.reopen(false)
	if e.gone(c.VMID) {
		t.Fatal("deleted before the deadline")
	}
	if r := mustRecord(t, e, c.VMID); !r.ExpiresAt.Equal(deadline) {
		t.Fatalf("deadline moved: %v -> %v", deadline, r.ExpiresAt)
	}
	e.eventually("deleted after restart", func() bool { return e.gone(c.VMID) })
	if now := time.Now(); now.Before(deadline) {
		t.Fatalf("deleted at %v before deadline %v", now, deadline)
	} else if now.After(deadline.Add(2 * time.Second)) {
		t.Fatalf("deleted at %v, long after the original deadline %v (reset by restart?)", now, deadline)
	}
	if n := e.lc.count("delete:"); n != 1 {
		t.Fatalf("delete calls = %d", n)
	}
}

// A restart AFTER the deadline (owner down while it passed) still expires once.
func TestTTLRestartAfterDeadlineExpiresOnce(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.createTTL("c1", 1)
	e.close()
	time.Sleep(1200 * time.Millisecond)
	e.reopen(false)
	e.eventually("expired after downtime", func() bool { return e.gone(c.VMID) })
	e.eventually("owner done", func() bool { return e.ownerStatus(c.VMID) == dbos.WorkflowStatusSuccess })
	if n := e.lc.count("delete:"); n != 1 {
		t.Fatalf("delete calls = %d", n)
	}
}

// A queued Delete admitted before the deadline keeps its order; expiry adds no
// second delete operation.
func TestTTLExplicitDeleteBeforeDeadlineKeepsOrder(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.createTTL("c1", 2)
	release := e.lc.arm("stop")
	defer release()
	stop := e.accept("s1", ControlRequest{Kind: "stop", VMID: c.VMID})
	e.lc.waitEntered(t, "stop")
	del := e.accept("d1", ControlRequest{Kind: "delete", VMID: c.VMID})
	time.Sleep(time.Until(*mustRecord(t, e, c.VMID).ExpiresAt) + 300*time.Millisecond)
	release()
	if got := awaitControl(t, e.ctl, ownerUser, stop.ID); got.Status != "succeeded" {
		t.Fatalf("stop %+v", got)
	}
	if got := awaitControl(t, e.ctl, ownerUser, del.ID); got.Status != "succeeded" {
		t.Fatalf("delete %+v", got)
	}
	e.eventually("owner done", func() bool { return e.ownerStatus(c.VMID) == dbos.WorkflowStatusSuccess })
	n := 0
	for _, o := range e.st.Snapshot().Operations {
		if o.Kind == "delete" {
			n++
		}
	}
	if n != 1 || e.lc.count("delete:") != 1 {
		t.Fatalf("delete ops %d backend deletes %d", n, e.lc.count("delete:"))
	}
}

// A failed expiry Delete retains the VM (no kill, no free) and does not spin: the
// owner waits out its normal idle period and an explicit Delete still works.
func TestTTLFailedExpiryDeleteRetainsVMWithoutBusyLoop(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.createTTL("c1", 1)
	e.lc.mu.Lock()
	e.lc.fail["delete"] = vmlib.ErrRetained
	e.lc.mu.Unlock()
	e.eventually("expiry delete attempted", func() bool { return e.lc.count("delete:") >= 1 })
	time.Sleep(800 * time.Millisecond)
	if n := e.lc.count("delete:"); n != 1 {
		t.Fatalf("failed expiry delete retried %d times in a loop", n)
	}
	if _, ok := e.record(c.VMID); !ok {
		t.Fatal("record dropped after failed delete")
	}
	if _, err := e.svc.Get(context.Background(), c.VMID); err != nil {
		t.Fatalf("VM freed after failed delete: %v", err)
	}
	e.lc.mu.Lock()
	delete(e.lc.fail, "delete")
	e.lc.mu.Unlock()
	del := e.accept("d-manual", ControlRequest{Kind: "delete", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, del.ID); got.Status != "succeeded" {
		t.Fatalf("explicit delete %+v", got)
	}
}

// Old owner histories: an owner workflow written by the baseline build replays
// through the new build with dbos.Patch=false (the unchanged call sequence), is
// never expired, and keeps processing commands; the already-run child is not
// re-executed.
func TestTTLOldOwnerHistoryReplaysWithoutExpiry(t *testing.T) {
	e := newOwnerEnv(t)
	e.reopen(true) // baseline owner body
	c := e.create("legacy-create")
	if r, _ := e.record(c.VMID); r.ExpiresAt == nil {
		t.Fatal("admission should have set a deadline in this build")
	}
	// Make it a legacy VM: no deadline (as created before this feature).
	if err := e.st.Update(func(s *State) {
		s.update(c.VMID, func(v *VMRecord) { v.ExpiresAt, v.TTLSeconds = nil, 0 })
	}); err != nil {
		t.Fatal(err)
	}
	stop := e.accept("legacy-stop", ControlRequest{Kind: "stop", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, stop.ID); got.Status != "succeeded" {
		t.Fatalf("stop %+v", got)
	}
	if hasStep(e.stepNames(c.VMID), "DBOS.patch-"+ownerTTLPatch) {
		t.Fatal("baseline owner must not contain the patch marker")
	}
	stops := e.lc.count("stop:")
	e.reopen(false) // new code replays the old history
	e.eventually("owner recovered", func() bool { return e.ownerStatus(c.VMID) == dbos.WorkflowStatusPending })
	time.Sleep(700 * time.Millisecond)
	if e.lc.count("stop:") != stops {
		t.Fatal("replay re-executed an already recorded child")
	}
	names := e.stepNames(c.VMID)
	if hasStep(names, "DBOS.patch-"+ownerTTLPatch) || hasStep(names, "expire") {
		t.Fatalf("old history must stay on the unpatched path: %v", names)
	}
	if e.gone(c.VMID) {
		t.Fatal("legacy VM without deadline expired")
	}
	del := e.accept("legacy-del", ControlRequest{Kind: "delete", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, del.ID); got.Status != "succeeded" {
		t.Fatalf("delete on replayed owner %+v", got)
	}
	e.eventually("owner done", func() bool { return e.ownerStatus(c.VMID) == dbos.WorkflowStatusSuccess })
}

// A VM stored without expires_at (legacy record) under a NEW owner never expires
// and is never given a deadline retroactively, even across restart.
func TestTTLLegacyRecordNeverExpiresRetroactively(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.create("c1")
	if err := e.st.Update(func(s *State) {
		s.update(c.VMID, func(v *VMRecord) { v.ExpiresAt, v.TTLSeconds = nil, 0 })
	}); err != nil {
		t.Fatal(err)
	}
	e.reopen(false)
	time.Sleep(1500 * time.Millisecond)
	r, ok := e.record(c.VMID)
	if !ok || r.ExpiresAt != nil || r.TTLSeconds != 0 {
		t.Fatalf("legacy record changed: %+v", r)
	}
	if v, _ := e.ctl.ControlVM(ownerUser, c.VMID); v.ExpiresAt != nil {
		t.Fatalf("legacy view must have no deadline: %+v", v)
	}
	stop := e.accept("s", ControlRequest{Kind: "stop", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, stop.ID); got.Status != "succeeded" {
		t.Fatalf("legacy VM accepts ordinary commands: %+v", got)
	}
}

// An admitted-but-not-started ordinary command whose turn comes after the
// deadline is refused by the owner (durably, in the existing begin step).
func TestTTLQueuedCommandRefusedAtBeginAfterDeadline(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.createTTL("c1", 2)
	release := e.lc.arm("commit") // keep the first command busy past the deadline
	defer release()
	cm := e.accept("cm", ControlRequest{Kind: "commit", VMID: c.VMID, Name: "snap"})
	e.lc.waitEntered(t, "commit")
	// A second ordinary command cannot be queued behind it (one unfinished command
	// per VM), so only the begin-time rule is exercised via a direct beginOp.
	time.Sleep(time.Until(*mustRecord(t, e, c.VMID).ExpiresAt) + 200*time.Millisecond)
	op := ControlOperation{ID: "op-ttlqueued", OwnerUserID: ownerUser, Key: "q", Digest: strings.Repeat("a", 64), Kind: "stop", VMID: c.VMID, Status: "pending", Exec: execOwner}
	if err := e.st.Update(func(s *State) { s.Operations = append(s.Operations, op) }); err != nil {
		t.Fatal(err)
	}
	plan, err := e.ctl.beginOp(c.VMID, OwnerCmd{OpID: op.ID, Kind: "stop", Digest: op.Digest})
	if err != nil || plan.Run {
		t.Fatalf("expired pending command must not run: %+v %v", plan, err)
	}
	if got := e.op(op.ID); got.Status != "failed" || got.ErrorCode != "vm_expired" {
		t.Fatalf("op %+v", got)
	}
	release()
	if got := awaitControl(t, e.ctl, ownerUser, cm.ID); got.Status != "succeeded" {
		t.Fatalf("commit %+v", got)
	}
}

// An adopted baseline owner may have no first checkpoint: only Recv's sleep
// at step 2. Inserting Patch at step 1 would corrupt this real SDK history.
func TestTTLOldAdoptedOwnerMidRecvReplays(t *testing.T) {
	e := newOwnerEnv(t)
	e.close()
	st, err := OpenControlStore(e.dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	id := "bot-0123456789abcdef"
	tok, _ := randToken()
	if _, err = e.svc.Create(context.Background(), vmlib.CreateRequest{ID: id, ImageID: "ubuntu", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	if err = st.Update(func(s *State) {
		s.Generation = 1
		s.VMs = append(s.VMs, VMRecord{ID: id, Token: tok, Phase: PhaseActive, Generation: 1, Label: "legacy", OwnerUserID: ownerUser, ImageID: "ubuntu"})
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	e.reopen(true)
	e.eventually("baseline owner sleeping", func() bool { return hasStep(e.stepNames(id), "DBOS.sleep") })
	if names := e.stepNames(id); len(names) != 1 || names[0] != "DBOS.sleep" {
		t.Fatalf("want only outstanding Recv sleep: %v", names)
	}
	e.reopen(false)
	stop := e.accept("adopted-stop", ControlRequest{Kind: "stop", VMID: id})
	if got := awaitControl(t, e.ctl, ownerUser, stop.ID); got.Status != "succeeded" {
		t.Fatalf("recovered adoption: %+v", got)
	}
	if hasStep(e.stepNames(id), "DBOS.patch-"+ownerTTLPatch) {
		t.Fatal("adopted owner must not shift sparse Recv history")
	}
	if r, ok := e.record(id); !ok || r.ExpiresAt != nil {
		t.Fatalf("legacy deadline changed: %+v", r)
	}
}

// Failed domain persistence must not hand an unrecorded command to the owner.
func TestTTLPlanExpiryWriteFailureReturnsNoCommand(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.create("expiry-write-failure")
	e.close() // no workflow may race the deliberately broken store path
	if err := e.st.Update(func(s *State) {
		s.update(c.VMID, func(v *VMRecord) { past := time.Now().Add(-time.Hour); v.ExpiresAt = &past })
	}); err != nil {
		t.Fatal(err)
	}
	original := e.st.dir
	e.st.dir = filepath.Join(e.dir, "missing", "store")
	defer func() { e.st.dir = original }()
	cmd, err := e.ctl.planExpiry(c.VMID)
	if err == nil {
		t.Fatal("failed persistence must propagate its error")
	}
	if cmd.OpID != "" {
		t.Fatalf("failed persistence returned unrecorded delete: %+v", cmd)
	}
	if e.ctl.expiryAttempts(c.VMID) != 0 {
		t.Fatal("failed persistence changed in-memory state")
	}
}

// ---- pinned SDK semantics the TTL design relies on ----

// The TTL design depends on pinned SDK behavior (Patch and Recv checkpointing).
// A version bump must re-prove them: fail loudly instead of silently trusting.
func TestDBOSSDKPinnedVersionForTTLSemantics(t *testing.T) {
	// Go test binaries may omit dependency build info. Query the resolved module,
	// including replacements, rather than silently skipping the version gate.
	b, err := exec.Command("go", "list", "-m", "-json", "github.com/dbos-inc/dbos-transact-golang").Output()
	if err != nil {
		t.Fatal(err)
	}
	var module struct {
		Version string
		Replace *json.RawMessage
	}
	if err := json.Unmarshal(b, &module); err != nil {
		t.Fatal(err)
	}
	if module.Version != "v1.8.0" || module.Replace != nil {
		t.Fatalf("DBOS module changed (%s): re-verify Patch/Recv semantics", b)
	}
}

var probeRecvTimeout atomic.Int64 // ns; read ONLY to build the Recv argument

func recvProbe(ctx dbos.Context, _ string) (string, error) {
	_, err := dbos.Recv[string](ctx, "t", time.Duration(probeRecvTimeout.Load()))
	var de *dbos.Error
	if errors.As(err, &de) && de.Code == dbos.ErrorCodeTimeout {
		return "timeout", nil
	}
	if err != nil {
		return "", err
	}
	return "message", nil
}

func probeContext(t *testing.T, dir string) dbos.Context {
	t.Helper()
	d, err := dbos.NewContext(context.Background(), dbos.Config{AppName: "ttlprobe", DatabaseURL: "sqlite:" + filepath.Join(dir, "p.db"), ExecutorID: "probe", ApplicationVersion: "probe-1"})
	if err != nil {
		t.Fatal(err)
	}
	dbos.RegisterWorkflow(d, recvProbe, dbos.WithWorkflowName("probe-recv"))
	if err = dbos.Launch(d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Recv checkpoints its deadline as an ABSOLUTE instant. A recovered Recv waits
// only the remainder of the original deadline and ignores a different timeout
// argument, so computing the argument from persisted expires_at/the clock cannot
// change the replayed history or extend a wait across restart.
func TestDBOSSDKRecvDeadlineIsDurableAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	probeRecvTimeout.Store(int64(1500 * time.Millisecond))
	d1 := probeContext(t, dir)
	start := time.Now()
	if _, err := dbos.RunWorkflow(d1, recvProbe, "x", dbos.WithWorkflowID("probe-1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	_ = dbos.Shutdown(d1, time.Second)

	probeRecvTimeout.Store(int64(time.Hour)) // the recovering run "computes" a very different argument
	d2 := probeContext(t, dir)
	defer func() { _ = dbos.Shutdown(d2, time.Second) }()
	h, err := dbos.RetrieveWorkflow[string](d2, "probe-1")
	if err != nil {
		t.Fatal(err)
	}
	type res struct {
		v string
		e error
	}
	ch := make(chan res, 1)
	go func() { v, e := h.GetResult(); ch <- res{v, e} }()
	select {
	case r := <-ch:
		if r.e != nil || r.v != "timeout" {
			t.Fatalf("result %+v", r)
		}
		if el := time.Since(start); el > 6*time.Second {
			t.Fatalf("recovered Recv waited %v: the recorded deadline was not honored", el)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("recovered Recv ignored its recorded absolute deadline")
	}
	// The completed workflow's recorded timeout is part of history: steps are fixed.
	steps, err := dbos.GetWorkflowSteps(d2, "probe-1")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.StepName)
	}
	if len(names) < 2 || names[0] != "DBOS.recv" && names[0] != "DBOS.sleep" {
		t.Fatalf("unexpected recv history %v", names)
	}
}

var patchDecision atomic.Value // string: last Patch decision observed by newProbe

func oldProbe(ctx dbos.Context, _ string) (string, error) {
	// Match the baseline Create owner: begin is recorded before Recv. Patch
	// cannot safely precede a bare interrupted Recv with an empty first slot.
	if _, err := ownerStep(ctx, "anchor", 1, func(context.Context) (bool, error) { return true, nil }); err != nil {
		return "", err
	}
	_, err := dbos.Recv[string](ctx, "t", 1500*time.Millisecond)
	var de *dbos.Error
	if errors.As(err, &de) && de.Code == dbos.ErrorCodeTimeout {
		return "done", nil
	}
	return "other", err
}

// newProbe is oldProbe with a dbos.Patch in front of the existing call sequence.
func newProbe(ctx dbos.Context, in string) (string, error) {
	p, err := dbos.Patch(ctx, "probe-patch")
	if err != nil {
		return "", err
	}
	if p {
		patchDecision.Store("patched")
	} else {
		patchDecision.Store("legacy")
	}
	return oldProbe(ctx, in)
}

func patchCtx(t *testing.T, dir string, fn func(dbos.Context, string) (string, error)) dbos.Context {
	t.Helper()
	d, err := dbos.NewContext(context.Background(), dbos.Config{AppName: "ttlpatch", DatabaseURL: "sqlite:" + filepath.Join(dir, "p.db"), ExecutorID: "probe", ApplicationVersion: "patch-1"})
	if err != nil {
		t.Fatal(err)
	}
	dbos.RegisterWorkflow(d, fn, dbos.WithWorkflowName("probe-flow"))
	if err = dbos.Launch(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func stepNamesOf(t *testing.T, d dbos.Context, id string) []string {
	t.Helper()
	steps, err := dbos.GetWorkflowSteps(d, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range steps {
		out = append(out, s.StepName)
	}
	return out
}

func awaitProbe(t *testing.T, d dbos.Context, id string) {
	t.Helper()
	h, err := dbos.RetrieveWorkflow[string](d, id)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := h.GetResult(); err != nil || out != "done" {
		t.Fatalf("probe %s: %q %v", id, out, err)
	}
}

// dbos.Patch: a workflow interrupted mid-Recv BEFORE the patch existed replays
// with Patch==false (its recorded call at that step is not the marker); a fresh
// workflow gets true and records the marker; a patched workflow that is
// recovered again still gets true. These are the three cases vmOwner relies on.
func TestDBOSSDKPatchSemantics(t *testing.T) {
	dir := t.TempDir()
	old := patchCtx(t, dir, oldProbe)
	if _, err := dbos.RunWorkflow(old, oldProbe, "x", dbos.WithWorkflowID("old-history")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = dbos.Shutdown(old, time.Second) // interrupted mid-Recv: PENDING

	nw := patchCtx(t, dir, newProbe) // new build recovers the old history
	awaitProbe(t, nw, "old-history")
	if got, _ := patchDecision.Load().(string); got != "legacy" {
		t.Fatalf("old history replay: Patch decision %q, want legacy", got)
	}
	if hasStep(stepNamesOf(t, nw, "old-history"), "DBOS.patch-probe-patch") {
		t.Fatal("legacy replay must not insert a patch marker")
	}

	if _, err := dbos.RunWorkflow(nw, newProbe, "x", dbos.WithWorkflowID("fresh")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got, _ := patchDecision.Load().(string); got != "patched" {
		t.Fatalf("fresh workflow: %q, want patched", got)
	}
	_ = dbos.Shutdown(nw, time.Second) // patched workflow interrupted mid-Recv
	patchDecision.Store("")
	nw2 := patchCtx(t, dir, newProbe)
	defer func() { _ = dbos.Shutdown(nw2, time.Second) }()
	awaitProbe(t, nw2, "fresh")
	if got, _ := patchDecision.Load().(string); got != "patched" {
		t.Fatalf("replay of a patched history: %q, want patched", got)
	}
	if n := stepNamesOf(t, nw2, "fresh"); len(n) == 0 || n[0] != "DBOS.patch-probe-patch" {
		t.Fatalf("patch marker must be the first recorded call: %v", n)
	}
}

// vmlibtest import is used by the helper env in owner_contract_test.go; keep the
// dependency explicit for readers of this file.
var _ = vmlibtest.New
