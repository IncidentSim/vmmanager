package vmbot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

var bg = WithUser(context.Background(), testUser)

func mustCreate(t *testing.T, e *env, name string) Status {
	t.Helper()
	s, err := e.ctl.Create(bg, name)
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return s
}

func labels(e *env) []string {
	var out []string
	for _, v := range e.store.Snapshot().VMs {
		out = append(out, v.Label)
	}
	return out
}

func TestCreateSeveralVMsUntilCapacityWithUniqueNames(t *testing.T) {
	e := newEnvMax(t, 2)
	a := mustCreate(t, e, "alpha")
	if a.Name != "alpha" || a.Generation != 1 {
		t.Fatalf("%+v", a)
	}
	if _, err := e.ctl.Create(bg, "alpha"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name must be refused: %v", err)
	}
	if len(e.store.Snapshot().VMs) != 1 || e.be.Count() != 1 {
		t.Fatal("a refused duplicate must leave no record and no VM")
	}
	b := mustCreate(t, e, "")
	if b.Name != "vm-2" || b.Generation != 2 {
		t.Fatalf("auto label: %+v", b)
	}
	if _, err := e.ctl.Create(bg, "gamma"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("third VM exceeds max_vms: %v", err)
	}
	if e.be.Count() != 2 {
		t.Fatalf("backend count %d", e.be.Count())
	}
	// deleting frees capacity; generations keep growing and are never reused
	if _, err := e.ctl.Delete(bg, Ref{Sel: "alpha"}); err != nil {
		t.Fatal(err)
	}
	c := mustCreate(t, e, "")
	if c.Generation != 3 || c.Name != "vm-3" {
		t.Fatalf("%+v", c)
	}
	// an auto label never collides with an existing user label
	if _, err := e.ctl.Delete(bg, Ref{Sel: "vm-2"}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, "vm-4")
	d, err := e.ctl.Delete(bg, Ref{Sel: "vm-3"})
	if err != nil || d.Name != "vm-3" {
		t.Fatalf("%+v %v", d, err)
	}
	if e2 := mustCreate(t, e, ""); e2.Name == "vm-4" || e2.Name == "" {
		t.Fatalf("auto label collided: %q (have %v)", e2.Name, labels(e))
	}
}

func TestInvalidNamesAreRejectedBeforeAnyEffect(t *testing.T) {
	e := newEnvMax(t, 2)
	for _, bad := range []string{"A", "a b", "a;b", "$(id)", "../x", "-x", "x-", "1x", "bot-abc", "ä", "a\nb", "--help", "x" + string(make([]byte, 30))} {
		if _, err := e.ctl.Create(bg, bad); !errors.Is(err, ErrBadName) {
			t.Errorf("%q: want ErrBadName got %v", bad, err)
		}
	}
	if len(e.store.Snapshot().VMs) != 0 || e.be.Count() != 0 || len(e.terms.started) != 0 {
		t.Fatal("rejected names must have no effect")
	}
	// the label never becomes a path component: sockets/known_hosts use the ID
	mustCreate(t, e, "ok-name")
	v := e.only(t)
	for _, ent := range e.terms.started {
		if ent.VMID != v.ID {
			t.Fatalf("terminal keyed by %q, want the ID", ent.VMID)
		}
	}
}

func TestConcurrentCreatesRespectCapacityAndUniqueNames(t *testing.T) {
	e := newEnvMax(t, 2)
	var wg sync.WaitGroup
	var ok, capErr, nameErr, other atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.ctl.Create(bg, "same")
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrNameTaken):
				nameErr.Add(1)
			case errors.Is(err, ErrCapacity):
				capErr.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || other.Load() != 0 || e.be.Count() != 1 {
		t.Fatalf("one name, one VM: ok=%d name=%d cap=%d other=%d vms=%d", ok.Load(), nameErr.Load(), capErr.Load(), other.Load(), e.be.Count())
	}
	ok.Store(0)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.ctl.Create(bg, ""); err == nil {
				ok.Add(1)
			} else if !errors.Is(err, ErrCapacity) {
				t.Errorf("unexpected %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || len(e.store.Snapshot().VMs) != 2 || e.be.Count() != 2 { // 1 + 1 = max_vms
		t.Fatalf("capacity exceeded: ok=%d records=%d vms=%d", ok.Load(), len(e.store.Snapshot().VMs), e.be.Count())
	}
	seenG, seenL := map[uint64]bool{}, map[string]bool{}
	for _, v := range e.store.Snapshot().VMs {
		if seenG[v.Generation] || seenL[v.Label] {
			t.Fatalf("duplicate generation/label: %+v", e.store.Snapshot().VMs)
		}
		seenG[v.Generation], seenL[v.Label] = true, true
	}
}

func TestAmbiguousCreateOfOneVMDoesNotEraseOrHideAnother(t *testing.T) {
	e := newEnvMax(t, 2)
	a := mustCreate(t, e, "alpha")
	e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		e.svc.Create(ctx, r) // really created, response lost
		return vmlib.VM{}, context.DeadlineExceeded
	}
	e.lc.get = func(ctx context.Context, id string) (vmlib.VM, error) {
		if id != a.VMID {
			return vmlib.VM{}, errors.New("unavailable")
		}
		return e.svc.Get(ctx, id)
	}
	b, err := e.ctl.Create(bg, "beta")
	if !errors.Is(err, ErrUncertain) || !b.Present || !b.Uncertain {
		t.Fatalf("want uncertain B: %+v %v", b, err)
	}
	list, err := e.ctl.List(bg)
	if err != nil || len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "beta" || !list[1].Uncertain || list[0].Uncertain {
		t.Fatalf("both VMs must be listed, only B uncertain: %+v %v", list, err)
	}
	if s, err := e.ctl.Status(bg, Ref{Sel: "alpha"}); err != nil || s.State != string(vmlib.StateRunning) {
		t.Fatalf("A unaffected: %+v %v", s, err)
	}
	if _, err := e.ctl.Terminal(bg, Ref{Sel: "alpha"}); err != nil {
		t.Fatalf("A's terminal unaffected: %v", err)
	}
	if _, err := e.ctl.Create(bg, "gamma"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("uncertain B keeps occupying its slot: %v", err)
	}
	e.restart(t)
	if len(e.store.Snapshot().VMs) != 2 {
		t.Fatal("restart must keep both records")
	}
	if err := e.ctl.Recover(bg); err == nil {
		t.Fatal("B is still uncertain")
	}
	e.lc.get = nil
	if err := e.ctl.Recover(bg); err != nil {
		t.Fatal(err)
	}
	for _, v := range e.store.Snapshot().VMs {
		if v.Phase != PhaseActive {
			t.Fatalf("recovery must adopt both: %+v", v)
		}
	}
}

func TestMissingSelectorRefusesDestructiveAndOtherActions(t *testing.T) {
	e := newEnvMax(t, 2)
	a := mustCreate(t, e, "alpha")
	b := mustCreate(t, e, "beta")
	nStarted := len(e.terms.started)
	if _, err := e.ctl.Delete(bg, Ref{}); !errors.Is(err, ErrNeedSelector) {
		t.Fatalf("delete without selector must be refused: %v", err)
	}
	if _, err := e.ctl.Terminal(bg, Ref{}); !errors.Is(err, ErrNeedSelector) {
		t.Fatalf("terminal without selector: %v", err)
	}
	if _, err := e.ctl.Status(bg, Ref{}); !errors.Is(err, ErrNeedSelector) {
		t.Fatalf("status without selector: %v", err)
	}
	if e.be.Count() != 2 || len(e.terms.revoked) != 0 || len(e.store.Snapshot().VMs) != 2 {
		t.Fatal("a refused ambiguous command must change nothing (never latest, never all)")
	}
	if len(e.terms.started) != nStarted {
		t.Fatal("nothing may be started either")
	}
	if _, err := e.ctl.Delete(bg, Ref{Sel: "nope"}); !errors.Is(err, ErrNoVM) {
		t.Fatalf("unknown selector: %v", err)
	}
	if _, err := e.ctl.Delete(bg, Ref{Sel: "ALPHA"}); !errors.Is(err, ErrNoVM) {
		t.Fatalf("selectors are exact: %v", err)
	}
	if e.be.Count() != 2 {
		t.Fatal("unknown selector deleted something")
	}
	// by ID, exactly
	if _, err := e.ctl.Delete(bg, Ref{Sel: b.VMID}); err != nil {
		t.Fatal(err)
	}
	if v := e.only(t); v.ID != a.VMID {
		t.Fatalf("wrong VM deleted: %+v", v)
	}
	// with a single VM left the selector may be omitted again
	if _, err := e.ctl.Terminal(bg, Ref{}); err != nil {
		t.Fatalf("single VM keeps convenient commands: %v", err)
	}
}

func TestStaleGenerationNeverRetargetsAReplacement(t *testing.T) {
	e := newEnvMax(t, 2)
	a := mustCreate(t, e, "alpha") // gen 1
	b := mustCreate(t, e, "beta")  // gen 2
	if _, err := e.ctl.Delete(bg, Ref{Gen: a.Generation}); err != nil {
		t.Fatal(err)
	}
	c := mustCreate(t, e, "alpha") // same NAME, new generation 3: a replacement
	if c.Generation != 3 || c.VMID == a.VMID {
		t.Fatalf("%+v", c)
	}
	for name, call := range map[string]func() error{
		"delete":   func() error { _, err := e.ctl.Delete(bg, Ref{Gen: 1}); return err },
		"terminal": func() error { _, err := e.ctl.Terminal(bg, Ref{Gen: 1}); return err },
		"status":   func() error { _, err := e.ctl.Status(bg, Ref{Gen: 1}); return err },
		"mismatch": func() error { _, err := e.ctl.Delete(bg, Ref{Gen: 2, Sel: "alpha"}); return err },
	} {
		if err := call(); !errors.Is(err, ErrStale) {
			t.Errorf("%s: want ErrStale got %v", name, err)
		}
	}
	if e.be.Count() != 2 || len(e.store.Snapshot().VMs) != 2 {
		t.Fatal("stale references touched a live VM")
	}
	if _, err := e.svc.Get(bg, c.VMID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(bg, b.VMID); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteOfOneVMRevokesOnlyThatVM(t *testing.T) {
	e := newEnvMax(t, 2)
	a := mustCreate(t, e, "alpha")
	b := mustCreate(t, e, "beta")
	if _, err := e.ctl.Delete(bg, Ref{Sel: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if len(e.terms.revoked) == 0 {
		t.Fatal("alpha must be revoked")
	}
	for _, id := range e.terms.revoked {
		if id != a.VMID {
			t.Fatalf("only alpha may be revoked: %v", e.terms.revoked)
		}
	}
	if v, ok := e.store.Snapshot().ByID(b.VMID); !ok || v.Phase != PhaseActive {
		t.Fatal("beta must be untouched")
	}
	if link, err := e.ctl.Terminal(bg, Ref{Sel: "beta"}); err != nil || link == "" {
		t.Fatalf("beta terminal: %v", err)
	}
}

func TestFailedOrRetainedDeleteDoesNotFreeItsOccupancy(t *testing.T) {
	e := newEnvMax(t, 2)
	a := mustCreate(t, e, "alpha")
	mustCreate(t, e, "beta")
	e.lc.del = func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
		if id == a.VMID {
			return vmlib.DeleteResult{}, context.DeadlineExceeded
		}
		return e.svc.Delete(ctx, id)
	}
	if _, err := e.ctl.Delete(bg, Ref{Sel: "alpha"}); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := e.ctl.Create(bg, "gamma"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("an unconfirmed delete must keep occupying: %v", err)
	}
	if v, _ := e.store.Snapshot().ByID(a.VMID); v.Phase != PhaseDeleting {
		t.Fatalf("%+v", v)
	}
	// B deletes fine while A is stuck; then there is room for exactly one more
	if _, err := e.ctl.Delete(bg, Ref{Sel: "beta"}); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, e, "gamma")
	if _, err := e.ctl.Create(bg, "delta"); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
}

func TestPerVMOperationsProgressIndependently(t *testing.T) {
	e := newEnvMax(t, 3)
	fast := mustCreate(t, e, "fast")
	release := make(chan struct{})
	entered := make(chan struct{})
	var first atomic.Bool
	e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return e.svc.Create(ctx, r)
	}
	slowDone := make(chan error, 1)
	go func() { _, err := e.ctl.Create(bg, "slow"); slowDone <- err }()
	<-entered

	// While "slow" is mid-Create: other VMs create, report, open terminals and delete.
	other, err := e.ctl.Create(bg, "other")
	if err != nil {
		t.Fatalf("a second create must not wait for the slow one: %v", err)
	}
	if _, err := e.ctl.Terminal(bg, Ref{Sel: "fast"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ctl.Delete(bg, Ref{Sel: "fast"}); err != nil {
		t.Fatal(err)
	}
	_ = fast
	list, err := e.ctl.List(bg)
	if err != nil || len(list) != 2 {
		t.Fatalf("both live VMs listed: %+v %v", list, err)
	}
	var sawSlowBusy bool
	for _, s := range list {
		if s.Name == "slow" {
			sawSlowBusy = s.Busy && s.Phase == PhaseCreating
		}
	}
	if !sawSlowBusy {
		t.Fatalf("the VM being created must be listed from durable state, marked busy: %+v", list)
	}
	if s, err := e.ctl.Status(bg, Ref{Sel: "other"}); err != nil || s.VMID != other.VMID {
		t.Fatal(err)
	}
	close(release)
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
	if v, _ := e.store.Snapshot().BySelector("slow"); v.Phase != PhaseActive {
		t.Fatalf("%+v", v)
	}
}

func TestRestartRecoversEachRecordIndependently(t *testing.T) {
	e := newEnvMax(t, 3)
	a := mustCreate(t, e, "alpha")
	b := mustCreate(t, e, "beta")
	c := mustCreate(t, e, "gamma")
	// gamma is mid-delete when the process dies
	e.lc.del = func(context.Context, string) (vmlib.DeleteResult, error) {
		return vmlib.DeleteResult{}, context.DeadlineExceeded
	}
	e.ctl.Delete(bg, Ref{Sel: "gamma"})
	e.restart(t)
	// alpha's remote state is unknown; the others are fine
	e.lc.get = func(ctx context.Context, id string) (vmlib.VM, error) {
		if id == a.VMID {
			return vmlib.VM{}, errors.New("timeout")
		}
		return e.svc.Get(ctx, id)
	}
	if err := e.ctl.Recover(bg); err == nil {
		t.Fatal("alpha's uncertainty must be reported")
	}
	snap := e.store.Snapshot()
	if len(snap.VMs) != 3 {
		t.Fatalf("one failure must not drop or hide others: %+v", snap.VMs)
	}
	started := map[string]bool{}
	for _, tg := range e.terms.started {
		started[tg.VMID] = true
	}
	if !started[b.VMID] || started[a.VMID] || started[c.VMID] {
		t.Fatalf("only beta's terminal may come up: %v", started)
	}
	revoked := map[string]bool{}
	for _, id := range e.terms.revoked {
		revoked[id] = true
	}
	if !revoked[c.VMID] || revoked[b.VMID] || revoked[a.VMID] {
		t.Fatalf("only the deleting VM is revoked: %v", e.terms.revoked)
	}
	if v, _ := snap.ByID(c.VMID); v.Phase != PhaseDeleting {
		t.Fatalf("%+v", v)
	}
	e.lc.get = nil
	e.lc.del = nil
	if err := e.ctl.Recover(bg); err != nil {
		t.Fatal(err)
	}
}

func TestEarlyNotFoundNeverAbandonsOldIdentityEvenWhenNewVMCreationIsLegit(t *testing.T) {
	e, sb, finished := pendingCreateEnvMax(t, 2)
	var oldID atomic.Value
	e.lc.get = func(ctx context.Context, id string) (vmlib.VM, error) {
		if v := oldID.Load(); v != nil && id == v.(string) {
			return vmlib.VM{}, vmlib.ErrNotFound // misleading: Create has not settled
		}
		return e.svc.Get(ctx, id)
	}
	e.ctl.cfg.OpTimeout = 300 * time.Millisecond
	e.lc.create = wrapFirst(e.lc.create, func(r vmlib.CreateRequest) { oldID.Store(r.ID) })
	_, err := e.ctl.Create(bg, "old")
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("want ErrUncertain, got %v", err)
	}
	old := e.store.Snapshot().VMs[0]
	if old.Phase != PhaseCreating {
		t.Fatalf("%+v", old)
	}
	// a DISTINCT new VM under capacity is legitimate and independent
	nw, err := e.ctl.Create(bg, "fresh")
	if err != nil {
		t.Fatalf("new VM below capacity must be allowed: %v", err)
	}
	if nw.VMID == old.ID {
		t.Fatal("must be a different identity")
	}
	// ...but the old identity is still owned, not abandoned, across restart too
	e.restart(t)
	e.ctl.cfg.OpTimeout = 300 * time.Millisecond
	if err := e.ctl.Recover(bg); err == nil {
		t.Fatal("old identity is unresolved")
	}
	if v, ok := e.store.Snapshot().ByID(old.ID); !ok || v.Phase != PhaseCreating || v.Token != old.Token {
		t.Fatalf("old identity must be preserved untouched: %+v", v)
	}
	if _, err := e.ctl.Create(bg, "third"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("the unresolved record still occupies: %v", err)
	}
	close(sb.proceed)
	<-finished
	e.ctl.cfg.OpTimeout = 2 * time.Second
	if err := e.ctl.Recover(bg); err != nil {
		t.Fatalf("once settled the barrier decides: %v", err)
	}
	if _, ok := e.store.Snapshot().ByID(nw.VMID); !ok {
		t.Fatal("the legit new VM must survive")
	}
}

// wrapFirst calls hook with the first create request, then delegates.
func wrapFirst(next func(context.Context, vmlib.CreateRequest) (vmlib.VM, error), hook func(vmlib.CreateRequest)) func(context.Context, vmlib.CreateRequest) (vmlib.VM, error) {
	var once sync.Once
	return func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		once.Do(func() { hook(r) })
		return next(ctx, r)
	}
}

func pendingCreateEnvMax(t *testing.T, maxVMs int) (*env, *slowPrepare, chan struct{}) {
	t.Helper()
	e := newEnvMax(t, maxVMs)
	sb := &slowPrepare{Backend: vmlibtest.New(3, "base"), entered: make(chan struct{}), proceed: make(chan struct{})}
	svc, err := vmlib.NewService(sb)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	e.lc.Lifecycle = svc
	finished := make(chan struct{})
	var first atomic.Bool
	e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		if !first.CompareAndSwap(false, true) {
			return svc.Create(ctx, r)
		}
		sctx, cancel := context.WithCancel(ctx)
		go func() { defer close(finished); svc.Create(sctx, r) }()
		<-sb.entered
		cancel()
		return vmlib.VM{}, context.DeadlineExceeded
	}
	return e, sb, finished
}

// ---- migration through the controller: the live VM survives ----

func TestMigratedLiveVMIsAdoptedNotRecreatedAndKeepsItsLink(t *testing.T) {
	be := vmlibtest.New(3, "base")
	svc, _ := vmlib.NewService(be)
	// the VM already exists on the backend under its v1 identity (synthetic stand-in)
	const liveID = "bot-c5d5cd1e8a7894eb"
	if _, err := svc.Create(bg, vmlib.CreateRequest{ID: liveID, ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	oldTok := testTok(424242)
	writeState(t, dir, v1File(7, 555, &VMRecord{OwnerUserID: testUser, ID: liveID, Token: oldTok, Phase: PhaseActive, Generation: 7}), 0o600)

	var creates, deletes atomic.Int32
	lc := &hookLC{Lifecycle: svc,
		create: func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
			creates.Add(1)
			return svc.Create(ctx, r)
		},
		del: func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
			deletes.Add(1)
			return svc.Delete(ctx, id)
		}}
	store, err := OpenStore(dir, testUser)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	terms := &fakeTerms{}
	ctl := NewController(lc, store, terms, ControllerConfig{AllowedUserIDs: []int64{testUser}, MaxVMsPerUser: 8, ImageID: "base", Profile: "small",
		PublicOrigin: "https://bot.example.test:8443", MaxVMs: 2, OpTimeout: 2 * time.Second, QueryTimeout: time.Second})
	if err := ctl.Start(bg); err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if err := ctl.Recover(bg); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 0 || deletes.Load() != 0 {
		t.Fatalf("migration must not create or delete: creates=%d deletes=%d", creates.Load(), deletes.Load())
	}
	if len(terms.started) != 1 || terms.started[0].VMID != liveID || terms.started[0].Token != oldTok {
		t.Fatalf("same capability must be served: %+v", terms.started)
	}
	link, err := ctl.Terminal(bg, Ref{})
	if err != nil || link != "https://bot.example.test:8443/t/"+oldTok+"/" {
		t.Fatalf("the existing link must be unchanged: %q %v", link, err)
	}
	if got := store.Snapshot(); got.Offset != 555 || got.Generation != 7 {
		t.Fatalf("%+v", got)
	}
	// another VM can now be added next to it; generation continues; old record intact
	n, err := ctl.Create(bg, "second")
	if err != nil || n.Generation != 8 {
		t.Fatalf("%+v %v", n, err)
	}
	if v, _ := store.Snapshot().ByGeneration(7); v.ID != liveID || v.Token != oldTok || v.Phase != PhaseActive {
		t.Fatalf("%+v", v)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.v1.bak")); err != nil {
		t.Fatal("backup must exist")
	}
}
