package vmbot

// Contract tests for the sequential durable VM owner (docs/rfc-vm-owner.md).
// They drive the real controller over real DBOS file SQLite and the in-memory
// vmlib fake. They prove control flow, ordering and durability of the
// application; they say nothing about live Firecracker.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

const ownerUser = int64(11)

// gateLC wraps a Lifecycle with per-method gates and a call log, so tests can
// hold one effect open and observe exactly which backend calls happened.
type gateLC struct {
	vmlib.Lifecycle
	mu    sync.Mutex
	calls []string
	// hold, when set for an op name ("stop","commit","delete","create"), blocks
	// that call (after logging "<op>:enter") until the returned channel closes.
	hold map[string]chan struct{}
	fail map[string]error
	// entered is closed the first time the named op enters.
	entered map[string]chan struct{}
	// active/maxActive count concurrent effect calls per VM (overlap detector).
	active, maxActive map[string]int
}

func newGateLC(l vmlib.Lifecycle) *gateLC {
	return &gateLC{Lifecycle: l, hold: map[string]chan struct{}{}, fail: map[string]error{}, entered: map[string]chan struct{}{}, active: map[string]int{}, maxActive: map[string]int{}}
}
func (g *gateLC) arm(op string) (release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := make(chan struct{})
	g.hold[op], g.entered[op] = ch, make(chan struct{})
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}
func (g *gateLC) waitEntered(t *testing.T, op string) {
	t.Helper()
	g.mu.Lock()
	ch := g.entered[op]
	g.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never entered", op)
	}
}
func (g *gateLC) enter(op, id string) error {
	g.mu.Lock()
	g.calls = append(g.calls, op+":"+id)
	h, ent, fe := g.hold[op], g.entered[op], g.fail[op]
	g.mu.Unlock()
	if ent != nil {
		select {
		case <-ent:
		default:
			close(ent)
		}
	}
	if h != nil {
		<-h
	}
	return fe
}

// track marks an effect call on a VM as running until the returned func runs.
func (g *gateLC) track(id string) func() {
	g.mu.Lock()
	g.active[id]++
	if g.active[id] > g.maxActive[id] {
		g.maxActive[id] = g.active[id]
	}
	g.mu.Unlock()
	return func() { g.mu.Lock(); g.active[id]--; g.mu.Unlock() }
}
func (g *gateLC) count(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}
func (g *gateLC) Create(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
	if err := g.enter("create", r.ID); err != nil {
		return vmlib.VM{}, err
	}
	return g.Lifecycle.Create(ctx, r)
}
func (g *gateLC) Stop(ctx context.Context, id string) (vmlib.VM, error) {
	defer g.track(id)()
	if err := g.enter("stop", id); err != nil {
		return vmlib.VM{}, err
	}
	return g.Lifecycle.Stop(ctx, id)
}
func (g *gateLC) Commit(ctx context.Context, id, img string) (vmlib.Image, error) {
	defer g.track(id)()
	if err := g.enter("commit", id); err != nil {
		return vmlib.Image{}, err
	}
	return g.Lifecycle.Commit(ctx, id, img)
}
func (g *gateLC) DeleteImage(ctx context.Context, id string) error {
	if err := g.enter("deleteimage", id); err != nil {
		return err
	}
	return g.Lifecycle.DeleteImage(ctx, id)
}
func (g *gateLC) Delete(ctx context.Context, id string) (vmlib.DeleteResult, error) {
	defer g.track(id)()
	if err := g.enter("delete", id); err != nil {
		return vmlib.DeleteResult{}, err
	}
	return g.Lifecycle.Delete(ctx, id)
}

type ownerEnv struct {
	t     *testing.T
	dir   string
	svc   *vmlib.Service
	lc    *gateLC
	terms *fakeTerms
	st    *Store
	ctl   *Controller
}

func newOwnerEnv(t *testing.T) *ownerEnv {
	t.Helper()
	svc, err := vmlib.NewService(vmlibtest.New(4, "ubuntu"))
	if err != nil {
		t.Fatal(err)
	}
	e := &ownerEnv{t: t, dir: filepath.Join(t.TempDir(), "state"), svc: svc, lc: newGateLC(svc), terms: &fakeTerms{}}
	e.open(true)
	t.Cleanup(func() { e.close() })
	return e
}

// open (re)opens the store and controller on the same directory; start=false
// leaves orchestration stopped so a test can inspect the Start rejection.
func (e *ownerEnv) open(start bool) {
	e.t.Helper()
	var err error
	if e.st, err = OpenControlStore(e.dir, 0); err != nil {
		e.t.Fatal(err)
	}
	e.ctl = NewController(e.lc, e.st, e.terms, ControllerConfig{Profile: "small", PublicOrigin: "https://t.example.test", OpTimeout: 3 * time.Second, QueryTimeout: time.Second})
	e.ctl.ConfigureControl(ControlPolicy{AllowedUsers: []int64{ownerUser, 22}, SharedImages: []string{"ubuntu"}, MaxVMs: 3, MaxVMsPerUser: 3})
	if !start {
		return
	}
	if err = e.ctl.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	if err = e.ctl.RecoverControl(); err != nil {
		e.t.Fatal(err)
	}
}
func (e *ownerEnv) close() {
	if e.ctl != nil {
		_ = e.ctl.Close()
	}
	if e.st != nil {
		e.st.Close()
	}
}
func (e *ownerEnv) accept(key string, req ControlRequest) ControlOperation {
	e.t.Helper()
	o, err := e.ctl.AcceptControl(ownerUser, key, req)
	if err != nil {
		e.t.Fatalf("accept %s: %v", key, err)
	}
	return o
}
func (e *ownerEnv) create(key string) ControlOperation {
	e.t.Helper()
	o := e.accept(key, ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if got := awaitControl(e.t, e.ctl, ownerUser, o.ID); got.Status != "succeeded" {
		e.t.Fatalf("create %+v", got)
	}
	return o
}
func (e *ownerEnv) op(id string) ControlOperation {
	o, err := e.ctl.ControlOperation(ownerUser, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}
func (e *ownerEnv) phase(vm string) string {
	v, err := e.ctl.ControlVM(ownerUser, vm)
	if err != nil {
		return "absent:" + err.Error()
	}
	return v.Phase
}
func (e *ownerEnv) ownerStatus(vm string) dbos.WorkflowStatusType {
	e.t.Helper()
	ws, err := dbos.ListWorkflows(e.ctl.engine().ctx, dbos.WithFilterWorkflowIDs(ownerWorkflowID(vm)))
	if err != nil || len(ws) != 1 {
		e.t.Fatalf("owner workflow for %s: %v (%d rows)", vm, err, len(ws))
	}
	return ws[0].Status
}
func (e *ownerEnv) eventually(what string, ok func() bool) {
	e.t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timeout: %s", what)
}

// Acceptance 1: full lifecycle through the real controller; the owner stays
// PENDING between operations and ends only after confirmed deletion.
func TestOwnerFullLifecycle(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	if got := e.ownerStatus(c.VMID); got != dbos.WorkflowStatusPending {
		t.Fatalf("owner status after create = %s", got)
	}
	if p := e.phase(c.VMID); p != PhaseActive {
		t.Fatalf("phase %s", p)
	}
	term := e.accept("t1", ControlRequest{Kind: "terminal", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, term.ID); got.Status != "succeeded" {
		t.Fatalf("terminal %+v", got)
	}
	if _, err := e.ctl.ControlTerminalURL(ownerUser, c.VMID); err != nil {
		t.Fatal(err)
	}
	stop := e.accept("s1", ControlRequest{Kind: "stop", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, stop.ID); got.Status != "succeeded" {
		t.Fatalf("stop %+v", got)
	}
	if p := e.phase(c.VMID); p != "stopped" {
		t.Fatalf("phase after stop %s", p)
	}
	for i, name := range []string{"golden-a", "golden-b"} { // repeated cold commits
		cm := e.accept(fmt.Sprintf("cm%d", i), ControlRequest{Kind: "commit", VMID: c.VMID, Name: name})
		if got := awaitControl(t, e.ctl, ownerUser, cm.ID); got.Status != "succeeded" {
			t.Fatalf("commit %d %+v", i, got)
		}
		if p := e.phase(c.VMID); p != "committed" {
			t.Fatalf("phase after commit %s", p)
		}
	}
	imgs, _ := e.svc.ListImages(context.Background())
	if len(imgs) != 3 { // the base image plus two independent commits
		t.Fatalf("want 3 images, have %d", len(imgs))
	}
	if got := e.ownerStatus(c.VMID); got != dbos.WorkflowStatusPending {
		t.Fatalf("owner must outlive commits, status %s", got)
	}
	del := e.accept("d1", ControlRequest{Kind: "delete", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, del.ID); got.Status != "succeeded" {
		t.Fatalf("delete %+v", got)
	}
	if p := e.phase(c.VMID); p == PhaseActive || p == "committed" {
		t.Fatalf("VM still visible: %s", p)
	}
	e.eventually("owner finishes after delete", func() bool { return e.ownerStatus(c.VMID) == dbos.WorkflowStatusSuccess })
}

// Acceptance 2: Delete is deliberately sequential. Delivered while Stop runs, it
// neither changes the applied phase, revokes access, nor touches the backend
// until the prior child has settled.
func TestOwnerDeleteWaitsForRunningChild(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	release := e.lc.arm("stop")
	defer release()
	stop := e.accept("s1", ControlRequest{Kind: "stop", VMID: c.VMID})
	e.lc.waitEntered(t, "stop")
	revokedBefore := len(e.terms.revoked)
	del := e.accept("d1", ControlRequest{Kind: "delete", VMID: c.VMID})
	time.Sleep(300 * time.Millisecond) // give a wrongly eager implementation time to act
	if p := e.phase(c.VMID); p == PhaseDeleting {
		t.Fatalf("admission mutated the applied phase to %q", p)
	}
	e.terms.mu.Lock()
	revokedAfter := len(e.terms.revoked)
	e.terms.mu.Unlock()
	if revokedAfter != revokedBefore {
		t.Fatalf("delete admission revoked access (%d -> %d)", revokedBefore, revokedAfter)
	}
	if n := e.lc.count("delete:"); n != 0 {
		t.Fatalf("backend delete ran %d times while stop was in flight", n)
	}
	if got := e.op(del.ID).Status; got != "pending" {
		t.Fatalf("delete op status %q, want pending", got)
	}
	if _, err := e.svc.Get(context.Background(), c.VMID); err != nil {
		t.Fatalf("workspace released before prior work settled: %v", err)
	}
	release()
	if got := awaitControl(t, e.ctl, ownerUser, stop.ID); got.Status != "succeeded" {
		t.Fatalf("stop %+v", got)
	}
	if got := awaitControl(t, e.ctl, ownerUser, del.ID); got.Status != "succeeded" {
		t.Fatalf("delete %+v", got)
	}
	e.lc.mu.Lock()
	defer e.lc.mu.Unlock()
	si, di := -1, -1
	for i, c := range e.lc.calls {
		if len(c) > 5 && c[:5] == "stop:" && si < 0 {
			si = i
		}
		if len(c) > 7 && c[:7] == "delete:" && di < 0 {
			di = i
		}
	}
	if si < 0 || di < 0 || si > di {
		t.Fatalf("effect order %v", e.lc.calls)
	}
}

// A second Delete (or ordinary op) must not grow an unbounded backlog.
func TestOwnerBacklogIsBounded(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	release := e.lc.arm("stop")
	defer release()
	e.accept("s1", ControlRequest{Kind: "stop", VMID: c.VMID})
	e.lc.waitEntered(t, "stop")
	e.accept("d1", ControlRequest{Kind: "delete", VMID: c.VMID})
	if _, err := e.ctl.AcceptControl(ownerUser, "d2", ControlRequest{Kind: "delete", VMID: c.VMID}); !errors.Is(err, ErrControlConflict) {
		t.Fatalf("second pending delete: %v", err)
	}
	if _, err := e.ctl.AcceptControl(ownerUser, "s2", ControlRequest{Kind: "stop", VMID: c.VMID}); !errors.Is(err, ErrControlConflict) {
		t.Fatalf("ordinary op behind pending ones: %v", err)
	}
}

// Acceptance 2: two VMs progress independently.
func TestOwnerTwoVMsIndependent(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	a, b := e.create("ca"), e.create("cb")
	release := e.lc.arm("stop")
	defer release()
	sa := e.accept("sa", ControlRequest{Kind: "stop", VMID: a.VMID})
	e.lc.waitEntered(t, "stop")
	// a's stop is blocked; b's Delete must still complete.
	e.lc.mu.Lock()
	delete(e.lc.hold, "stop")
	e.lc.mu.Unlock()
	db := e.accept("db", ControlRequest{Kind: "delete", VMID: b.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, db.ID); got.Status != "succeeded" {
		t.Fatalf("b delete %+v", got)
	}
	if got := e.op(sa.ID).Status; got == "succeeded" {
		t.Fatal("a's stop should still be blocked")
	}
	release()
	awaitControl(t, e.ctl, ownerUser, sa.ID)
}

// Acceptance 3: a duplicate request attaches to one semantic operation; a
// changed payload under the same key is rejected before any effect; the
// rejection survives deletion of the VM.
func TestOwnerDuplicateAndChangedPayload(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	s1 := e.accept("s", ControlRequest{Kind: "stop", VMID: c.VMID})
	s2 := e.accept("s", ControlRequest{Kind: "stop", VMID: c.VMID})
	if s1.ID != s2.ID {
		t.Fatal("duplicate created a second operation")
	}
	awaitControl(t, e.ctl, ownerUser, s1.ID)
	if n := e.lc.count("stop:"); n != 1 {
		t.Fatalf("duplicate caused %d backend stops", n)
	}
	before := e.lc.count("delete:")
	if _, err := e.ctl.AcceptControl(ownerUser, "s", ControlRequest{Kind: "delete", VMID: c.VMID}); !errors.Is(err, ErrControlConflict) {
		t.Fatalf("changed kind under same key: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if e.lc.count("delete:") != before || e.phase(c.VMID) == PhaseDeleting {
		t.Fatal("rejected payload had a destructive side effect")
	}
	del := e.accept("d", ControlRequest{Kind: "delete", VMID: c.VMID})
	awaitControl(t, e.ctl, ownerUser, del.ID)
	if _, err := e.ctl.AcceptControl(ownerUser, "s", ControlRequest{Kind: "commit", VMID: c.VMID, Name: "x"}); !errors.Is(err, ErrControlConflict) {
		t.Fatalf("digest mismatch after removal: %v", err)
	}
	again := e.accept("d", ControlRequest{Kind: "delete", VMID: c.VMID})
	if again.ID != del.ID || again.Status != "succeeded" {
		t.Fatalf("completed duplicate: %+v", again)
	}
}

// Acceptance 5: an expected child failure is an outcome; the owner stays usable.
func TestOwnerExpectedFailureLeavesOwnerUsable(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	e.lc.mu.Lock()
	e.lc.fail["stop"] = vmlib.ErrResourceExhausted
	e.lc.mu.Unlock()
	s := e.accept("s1", ControlRequest{Kind: "stop", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, s.ID); got.Status != "failed" || got.ErrorCode != "capacity" {
		t.Fatalf("failed stop %+v", got)
	}
	if got := e.ownerStatus(c.VMID); got != dbos.WorkflowStatusPending {
		t.Fatalf("owner status %s after expected failure", got)
	}
	e.lc.mu.Lock()
	delete(e.lc.fail, "stop")
	e.lc.mu.Unlock()
	s2 := e.accept("s2", ControlRequest{Kind: "stop", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, s2.ID); got.Status != "succeeded" {
		t.Fatalf("retry %+v", got)
	}
}

// Acceptance 5: a terminal owner is visible, retains the VM and capacity, is not
// a silent success and is never replaced automatically.
func TestOwnerTerminalErrorVisibleAndRetained(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	if err := dbos.CancelWorkflow(e.ctl.engine().ctx, ownerWorkflowID(c.VMID)); err != nil {
		t.Fatal(err)
	}
	e.eventually("owner cancelled", func() bool { return e.ownerStatus(c.VMID) == dbos.WorkflowStatusCancelled })
	if _, err := e.ctl.AcceptControl(ownerUser, "d1", ControlRequest{Kind: "delete", VMID: c.VMID}); !errors.Is(err, ErrOwnerUnavailable) {
		t.Fatalf("command for dead owner: %v", err)
	}
	if v, err := e.ctl.ControlVM(ownerUser, c.VMID); err != nil || v.Phase == "deleted" {
		t.Fatalf("VM must stay visible: %+v %v", v, err)
	}
	if _, err := e.svc.Get(context.Background(), c.VMID); err != nil {
		t.Fatal("resource released by a dead owner")
	}
	if err := e.ctl.RecoverControl(); err == nil {
		t.Fatal("recovery must report the dead owner, not hide it")
	}
	ws, _ := dbos.ListWorkflows(e.ctl.engine().ctx, dbos.WithFilterName(ownerWorkflowName))
	if len(ws) != 1 {
		t.Fatalf("automatic replacement owner appeared: %d", len(ws))
	}
	if len(e.st.Snapshot().VMs) != 1 {
		t.Fatal("capacity freed")
	}
}

// Acceptance 6: an existing settled record is adopted without a Create.
func TestOwnerAdoptsSettledRecordWithoutCreate(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	e.close()
	// Fabricate a settled record as an earlier (pre-owner) build would have left it.
	svc, _ := vmlib.NewService(vmlibtest.New(4, "ubuntu"))
	e.svc, e.lc = svc, newGateLC(svc)
	if _, err := svc.Create(context.Background(), vmlib.CreateRequest{ID: "bot-0123456789abcdef", ImageID: "ubuntu", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	st, err := OpenControlStore(e.dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := randToken()
	if err := st.Update(func(s *State) {
		s.Generation = 1
		s.VMs = append(s.VMs, VMRecord{ID: "bot-0123456789abcdef", Token: tok, Phase: PhaseActive, Generation: 1, Label: "old", OwnerUserID: ownerUser, ImageID: "ubuntu"})
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	e.open(true)
	if got := e.ownerStatus("bot-0123456789abcdef"); got != dbos.WorkflowStatusPending {
		t.Fatalf("adopted owner status %s", got)
	}
	if n := e.lc.count("create:"); n != 0 {
		t.Fatalf("adoption called Create %d times", n)
	}
	v, err := e.ctl.ControlVM(ownerUser, "bot-0123456789abcdef")
	if err != nil || v.Phase != PhaseActive || v.Name != "old" {
		t.Fatalf("identity not preserved: %+v %v", v, err)
	}
	s := e.accept("s", ControlRequest{Kind: "stop", VMID: "bot-0123456789abcdef"})
	if got := awaitControl(t, e.ctl, ownerUser, s.ID); got.Status != "succeeded" {
		t.Fatalf("adopted owner cannot stop: %+v", got)
	}
}

// Acceptance 6: unsafe pending legacy state is rejected at startup with a
// diagnostic; nothing is dropped, replaced or created.
func TestOwnerRejectsPendingLegacyCreate(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	e.close()
	st, err := OpenControlStore(e.dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	digest := "0000000000000000000000000000000000000000000000000000000000000000"
	tok, _ := randToken()
	if err := st.Update(func(s *State) {
		s.Generation = 1
		s.VMs = append(s.VMs, VMRecord{ID: "bot-aaaaaaaaaaaaaaaa", Token: tok, Phase: PhaseCreating, Generation: 1, Label: "pending", OwnerUserID: ownerUser, ImageID: "ubuntu"})
		s.Operations = append(s.Operations, ControlOperation{ID: "op-aaaaaaaaaaaaaaaa", OwnerUserID: ownerUser, Key: "k", Digest: digest, Kind: "create", VMID: "bot-aaaaaaaaaaaaaaaa", Status: "pending"})
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	e.open(false)
	err = e.ctl.Start(context.Background())
	if err == nil {
		t.Fatal("startup must reject a pending legacy create")
	}
	for _, want := range []string{"bot-aaaaaaaaaaaaaaaa", "op-aaaaaaaaaaaaaaaa"} {
		if !contains(err.Error(), want) {
			t.Fatalf("diagnostic %q lacks %q", err, want)
		}
	}
	if e.lc.count("") != 0 {
		t.Fatalf("rejected handoff touched the backend: %v", e.lc.calls)
	}
	if len(e.st.Snapshot().VMs) != 1 || len(e.st.Snapshot().Operations) != 1 {
		t.Fatal("rejected handoff dropped state")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Acceptance 5: an unexpected owner failure (here a protocol violation) is a
// terminal ERROR that is visible, retains the VM and capacity, never implies
// deletion, and is not replaced automatically.
func TestOwnerUnexpectedErrorIsVisibleAndRetained(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	if err := dbos.Send(e.ctl.engine().ctx, ownerWorkflowID(c.VMID), OwnerCmd{OpID: "op-bogus", Kind: "create"}, ownerCmdTopic); err != nil {
		t.Fatal(err)
	}
	e.eventually("owner ERROR", func() bool { return e.ownerStatus(c.VMID) == dbos.WorkflowStatusError })
	v, err := e.ctl.ControlVM(ownerUser, c.VMID)
	if err != nil || !v.OwnerError || v.Phase != PhaseActive {
		t.Fatalf("view must show a retained VM with a failed owner: %+v %v", v, err)
	}
	if _, err := e.ctl.AcceptControl(ownerUser, "x", ControlRequest{Kind: "stop", VMID: c.VMID}); !errors.Is(err, ErrOwnerUnavailable) {
		t.Fatalf("command for ERROR owner: %v", err)
	}
	if err := e.ctl.RecoverControl(); err == nil {
		t.Fatal("recovery must report the ERROR owner")
	}
	if _, err := e.svc.Get(context.Background(), c.VMID); err != nil {
		t.Fatal("resource released by an ERROR owner")
	}
	ws, _ := dbos.ListWorkflows(e.ctl.engine().ctx, dbos.WithFilterName(ownerWorkflowName))
	if len(ws) != 1 {
		t.Fatalf("replacement owner appeared: %d", len(ws))
	}
	// Capacity stays consumed (MaxVMs 3, this VM still counts).
	for i := 0; i < 2; i++ {
		e.accept(fmt.Sprintf("fill%d", i), ControlRequest{Kind: "create", ImageID: "ubuntu"})
	}
	if _, err := e.ctl.AcceptControl(ownerUser, "over", ControlRequest{Kind: "create", ImageID: "ubuntu"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity was freed by a dead owner: %v", err)
	}
}

// Focused race: contenders on two VMs keep admitting stop/commit/terminal for a
// few seconds, waiting for each accepted operation; a Delete lands at the end.
// Whatever is admitted, same-VM backend effects never overlap.
func TestOwnerRaceNoOverlapAndBoundedBacklog(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	a, b := e.create("ca"), e.create("cb")
	var wg sync.WaitGroup
	var admitted, conflicts atomic.Int32
	stopAt := time.Now().Add(4 * time.Second)
	for g := 0; g < 5; g++ {
		for _, vm := range []string{a.VMID, b.VMID} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				kinds := []string{"stop", "commit", "terminal"}
				for i := 0; time.Now().Before(stopAt); i++ {
					k := kinds[(g+i)%3]
					req := ControlRequest{Kind: k, VMID: vm}
					if k == "commit" {
						req.Name = fmt.Sprintf("img-%d-%d-%s", g, i, vm[len(vm)-4:])
					}
					o, err := e.ctl.AcceptControl(ownerUser, fmt.Sprintf("k-%s-%d-%d", vm, g, i), req)
					if err != nil {
						conflicts.Add(1)
						time.Sleep(5 * time.Millisecond)
						continue
					}
					admitted.Add(1)
					for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
						if got, _ := e.ctl.ControlOperation(ownerUser, o.ID); got.Status == "succeeded" || got.Status == "failed" {
							break
						}
					}
				}
			}()
		}
	}
	wg.Wait()
	for _, vm := range []string{a.VMID, b.VMID} {
		d := e.accept("final-delete-"+vm, ControlRequest{Kind: "delete", VMID: vm})
		awaitControl(t, e.ctl, ownerUser, d.ID)
	}
	e.lc.mu.Lock()
	defer e.lc.mu.Unlock()
	for vm, m := range e.lc.maxActive {
		if m > 1 {
			t.Fatalf("effects overlapped on %s (max %d)", vm, m)
		}
	}
	if admitted.Load() < 4 {
		t.Fatalf("race did not exercise the owners: admitted=%d", admitted.Load())
	}
	t.Logf("admitted=%d rejected-or-conflicting=%d", admitted.Load(), conflicts.Load())
}

// Positive control for the overlap detector: two concurrent calls are seen.
func TestOwnerOverlapDetectorSeesOverlap(t *testing.T) {
	t.Parallel()
	g := newGateLC(nil)
	done1, done2 := g.track("vm"), g.track("vm")
	done1()
	done2()
	if g.maxActive["vm"] != 2 {
		t.Fatal("detector cannot see overlap")
	}
}

// Concurrent duplicates of one request attach to one operation and one effect.
func TestOwnerRaceConcurrentDuplicatesOneEffect(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("c1")
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := e.ctl.AcceptControl(ownerUser, "same", ControlRequest{Kind: "stop", VMID: c.VMID})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- o.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("duplicates produced several operations: %s vs %s", id, first)
		}
	}
	awaitControl(t, e.ctl, ownerUser, first)
	if n := e.lc.count("stop:"); n != 1 {
		t.Fatalf("duplicate effect count %d", n)
	}
}

// Concurrent creates cannot exceed the per-user quota.
func TestOwnerRaceQuota(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t) // MaxVMsPerUser 3
	var wg sync.WaitGroup
	var ok, full int32
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.ctl.AcceptControl(ownerUser, fmt.Sprintf("q%d", i), ControlRequest{Kind: "create", ImageID: "ubuntu"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrCapacity):
				full++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 3 || full != 7 {
		t.Fatalf("admitted %d rejected %d", ok, full)
	}
}

// A graceful shutdown that interrupts an in-flight child must not poison the
// owner or record a false outcome: after restart the owner is PENDING again and
// the command completes (the effect may rerun; no exactly-once claim).
func TestOwnerGracefulShutdownDuringChildResumes(t *testing.T) {
	e := newOwnerEnv(t)
	c := e.create("c1")
	release := e.lc.arm("stop")
	defer release()
	stop := e.accept("s1", ControlRequest{Kind: "stop", VMID: c.VMID})
	e.lc.waitEntered(t, "stop")
	closed := make(chan error, 1)
	go func() { closed <- e.ctl.Close() }()
	time.Sleep(300 * time.Millisecond)
	release() // let the in-flight effect return while shutdown is in progress
	select {
	case err := <-closed:
		t.Logf("close: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("close did not return")
	}
	e.st.Close()
	e.lc.mu.Lock()
	delete(e.lc.hold, "stop")
	e.lc.mu.Unlock()
	e.open(true)
	got := awaitControl(t, e.ctl, ownerUser, stop.ID)
	if got.Status != "succeeded" {
		t.Fatalf("interrupted stop did not complete after restart: %+v", got)
	}
	if st := e.ownerStatus(c.VMID); st != dbos.WorkflowStatusPending {
		t.Fatalf("owner status after graceful restart: %s", st)
	}
	if p := e.phase(c.VMID); p != "stopped" {
		t.Fatalf("phase %s", p)
	}
}
