package vmbot

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// panicOnRevoke simulates the process dying right after the terminal revoke.
type panicOnRevoke struct{ *fakeTerms }

func (f *panicOnRevoke) Revoke(id string) { f.fakeTerms.Revoke(id); panic("simulated process crash") }

func TestRevocationSurvivesCrashDuringDelete(t *testing.T) {
	e := newEnv(t)
	if _, err := e.ctl.Create(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	old := e.only(t).Token
	e.ctl.term = &panicOnRevoke{e.terms}
	func() {
		defer func() { _ = recover() }()
		e.ctl.Delete(context.Background(), Ref{})
	}()
	if v := e.vm(); v == nil || v.Phase != PhaseDeleting {
		t.Fatalf("deleting phase must be durable before the revoke: %+v", v)
	}
	e.restart(t)
	if err := e.ctl.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tg := range e.terms.started {
		if tg.Token == old {
			t.Fatal("restart re-authorized the revoked capability")
		}
	}
	if _, err := e.ctl.Terminal(context.Background(), Ref{}); err == nil {
		t.Fatal("no terminal may be served for a VM in the deleting phase")
	}
	// Recovery executes the durable delete, not just the terminal revoke.
	if e.vm() != nil || e.be.Count() != 0 {
		t.Fatal("recovery must complete deletion without leaving a resource")
	}
	if _, err := e.ctl.Delete(context.Background(), Ref{}); !errors.Is(err, ErrNoVM) {
		t.Fatalf("completed deletion must not target another VM: %v", err)
	}
}

func TestDeletePersistFailureChangesNothing(t *testing.T) {
	e := newEnv(t)
	st, err := e.ctl.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	good := e.store.dir
	e.store.dir = filepath.Join(good, "missing") // every persist fails
	_, derr := e.ctl.Delete(context.Background(), Ref{})
	e.store.dir = good
	if !errors.Is(derr, ErrFailed) {
		t.Fatalf("want ErrFailed, got %v", derr)
	}
	if len(e.terms.revoked) != 0 {
		t.Fatalf("no revocation may be claimed when the deleting phase is not durable: %v", e.terms.revoked)
	}
	if e.be.Count() != 1 {
		t.Fatal("no remote deletion may happen")
	}
	if v := e.vm(); v == nil || v.Phase != PhaseActive || v.ID != st.VMID {
		t.Fatalf("state must be unchanged: %+v", v)
	}
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err != nil {
		t.Fatalf("delete works once persistence recovers: %v", err)
	}
}

func TestRestartInDeletingPhaseNeverRestartsTerminal(t *testing.T) {
	e := newEnv(t)
	st, _ := e.ctl.Create(context.Background(), "")
	e.lc.del = func(context.Context, string) (vmlib.DeleteResult, error) {
		return vmlib.DeleteResult{}, context.DeadlineExceeded
	}
	e.ctl.Delete(context.Background(), Ref{})
	e.restart(t)
	if err := e.ctl.Recover(context.Background()); err == nil {
		t.Fatal("failed recovered delete must report an unconfirmed outcome")
	}
	if len(e.terms.started) != 0 {
		t.Fatalf("terminal restarted in deleting phase: %+v", e.terms.started)
	}
	if len(e.terms.revoked) == 0 {
		t.Fatal("leftovers must be revoked on recovery")
	}
	if v := e.vm(); v == nil || v.ID != st.VMID || v.Phase != PhaseDeleting {
		t.Fatalf("%+v", v)
	}
	if _, err := e.ctl.Create(context.Background(), ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("slot must stay occupied: %v", err)
	}
}

// slowPrepare blocks the FIRST Prepare until released and can make Release fail.
type slowPrepare struct {
	vmlib.Backend
	mu       sync.Mutex
	first    string
	entered  chan struct{}
	proceed  chan struct{}
	failRel  atomic.Bool
	released atomic.Int32
}

func (b *slowPrepare) Prepare(ctx context.Context, r vmlib.PrepareRequest) (vmlib.HostVM, error) {
	b.mu.Lock()
	first := b.first == ""
	if first {
		b.first = r.VMID
	}
	b.mu.Unlock()
	if first {
		close(b.entered)
		<-b.proceed
	}
	return b.Backend.Prepare(ctx, r)
}

func (b *slowPrepare) Release(ctx context.Context, id string) (vmlib.ReleaseResult, error) {
	b.mu.Lock()
	first := b.first
	b.mu.Unlock()
	if id == first && b.failRel.Load() {
		return vmlib.ReleaseResult{}, errors.New("release response uncertain")
	}
	b.released.Add(1)
	return b.Backend.Release(ctx, id)
}

func pendingCreateEnv(t *testing.T) (*env, *slowPrepare, chan struct{}) {
	e := newEnv(t)
	sb := &slowPrepare{Backend: vmlibtest.New(3, "base"), entered: make(chan struct{}), proceed: make(chan struct{})}
	svc, err := vmlib.NewService(sb)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	e.lc.Lifecycle = svc
	finished := make(chan struct{})
	first := true
	e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		if !first {
			return svc.Create(ctx, r)
		}
		first = false
		sctx, cancel := context.WithCancel(ctx)
		go func() { defer close(finished); svc.Create(sctx, r) }()
		<-sb.entered
		cancel() // the client's RPC ends while the server-side Prepare is still running
		return vmlib.VM{}, context.DeadlineExceeded
	}
	return e, sb, finished
}

func TestNotFoundDuringPendingCreateKeepsOccupancy(t *testing.T) {
	e, sb, finished := pendingCreateEnv(t)
	sb.failRel.Store(true)
	_, err := e.ctl.Create(context.Background(), "")
	if err == nil {
		t.Fatal("expected the first RPC to fail")
	}
	if e.vm() == nil {
		t.Fatal("slot released while the original Create may still be running")
	}
	if _, err := e.ctl.Create(context.Background(), ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("replacement create must be refused: %v", err)
	}
	close(sb.proceed)
	<-finished
	all, err := e.svc.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != e.vm().ID {
		t.Fatalf("exactly the durable owner must exist: %+v", all)
	}
	// The original settled as retained (release uncertain). Delete keeps it
	// occupied until the release is confirmed, then frees the slot.
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err == nil {
		t.Fatal("delete must not release while the release is unconfirmed")
	}
	if e.vm() == nil {
		t.Fatal("retained VM abandoned")
	}
	sb.failRel.Store(false)
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err != nil {
		t.Fatal(err)
	}
	if e.vm() != nil {
		t.Fatal("slot must be free after confirmed delete")
	}
}

// A lifecycle that (wrongly) answers NotFound while Create is unsettled must not
// make the app drop ownership: only the Delete barrier can.
func TestMisleadingNotFoundNeverReleasesBeforeBarrier(t *testing.T) {
	e, sb, finished := pendingCreateEnv(t)
	e.lc.get = func(context.Context, string) (vmlib.VM, error) { return vmlib.VM{}, vmlib.ErrNotFound }
	e.ctl.cfg.OpTimeout = 300 * time.Millisecond
	_, err := e.ctl.Create(context.Background(), "")
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("want ErrUncertain, got %v", err)
	}
	if v := e.vm(); v == nil || v.Phase != PhaseCreating {
		t.Fatalf("must stay in creating phase: %+v", v)
	}
	if _, err := e.ctl.Create(context.Background(), ""); err == nil {
		t.Fatal("second create must be refused")
	}
	// restart while still unsettled: same conclusion, still occupied.
	e.restart(t)
	e.ctl.cfg.OpTimeout = 300 * time.Millisecond
	if err := e.ctl.Recover(context.Background()); err == nil {
		t.Fatal("recovery must report uncertainty while the barrier cannot settle")
	}
	if v := e.vm(); v == nil || v.Phase != PhaseCreating {
		t.Fatalf("restart must keep the creating phase: %+v", v)
	}
	close(sb.proceed)
	<-finished
	e.ctl.cfg.OpTimeout = 2 * time.Second
	if err := e.ctl.Recover(context.Background()); err != nil {
		t.Fatalf("once settled the barrier proves the outcome: %v", err)
	}
	if e.vm() != nil || e.be.Count() != 0 {
		// the barrier deleted the unconfirmed VM; nothing may be left behind
		all, _ := e.svc.List(context.Background())
		t.Fatalf("slot/vms not clean: %+v %+v", e.vm(), all)
	}
}

func TestCreatingPhaseWithUnknownRemoteStaysOccupiedAcrossRestart(t *testing.T) {
	e := newEnv(t)
	e.lc.create = func(context.Context, vmlib.CreateRequest) (vmlib.VM, error) {
		return vmlib.VM{}, context.DeadlineExceeded
	}
	e.lc.get = func(context.Context, string) (vmlib.VM, error) { return vmlib.VM{}, vmlib.ErrNotFound }
	e.lc.del = func(context.Context, string) (vmlib.DeleteResult, error) {
		return vmlib.DeleteResult{}, errors.New("unreachable")
	}
	if _, err := e.ctl.Create(context.Background(), ""); !errors.Is(err, ErrUncertain) {
		t.Fatalf("got %v", err)
	}
	e.restart(t)
	if err := e.ctl.Recover(context.Background()); err == nil {
		t.Fatal("expected uncertainty")
	}
	if e.vm() == nil {
		t.Fatal("ownership dropped on NotFound without barrier proof")
	}
	if _, err := e.ctl.Create(context.Background(), ""); err == nil {
		t.Fatal("new VM allowed while the old create is unresolved")
	}
}

// errPrepare makes every Prepare fail with a transport-style error while the
// backend holds nothing: for vmlib this is an ambiguous, unprovable outcome.
type errPrepare struct{ vmlib.Backend }

func (errPrepare) Prepare(context.Context, vmlib.PrepareRequest) (vmlib.HostVM, error) {
	return vmlib.HostVM{}, errors.New("transport broke")
}

func TestUnprovableAmbiguousPrepareKeepsSlotUntilOperatorRecovery(t *testing.T) {
	e := newEnv(t)
	svc, err := vmlib.NewService(errPrepare{vmlibtest.New(3, "base")})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	e.lc.Lifecycle = svc
	st, err := e.ctl.Create(context.Background(), "")
	if err == nil || !st.Present {
		t.Fatalf("the unproven identity must stay owned: %+v %v", st, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.ctl.Delete(context.Background(), Ref{}); err == nil {
			t.Fatal("delete must not release an unproven ambiguous prepare")
		}
		if e.vm() == nil {
			t.Fatal("slot released without backend proof")
		}
	}
	if _, err := e.ctl.Create(context.Background(), ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("no second VM while unresolved: %v", err)
	}
	e.restart(t) // operator recovery boundary: restarting the controller alone changes nothing
	if err := e.ctl.Recover(context.Background()); err == nil && e.vm() == nil {
		t.Fatal("restart must not release the slot")
	}
}
