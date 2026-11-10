package vmlib_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// gatedBackend holds Prepare until released; failures are injectable. Inspect can
// park once after answering, to interleave operations deterministically.
type gatedBackend struct {
	*vmlibtest.Backend
	entered    chan struct{}
	proceed    chan struct{}
	prepareErr error
	errAfter   bool // Prepare really succeeds on the backend but reports a transport error
	inspGate   atomic.Bool
	inspected  chan struct{}
	resume     chan struct{}
}

func newGated() *gatedBackend {
	return &gatedBackend{Backend: vmlibtest.New(3, "base"), entered: make(chan struct{}, 4), proceed: make(chan struct{})}
}

func (g *gatedBackend) Prepare(ctx context.Context, r vmlib.PrepareRequest) (vmlib.HostVM, error) {
	g.entered <- struct{}{}
	<-g.proceed
	if g.errAfter {
		_, _ = g.Backend.Prepare(ctx, r)
		return vmlib.HostVM{}, g.prepareErr
	}
	if g.prepareErr != nil {
		return vmlib.HostVM{}, g.prepareErr
	}
	return g.Backend.Prepare(ctx, r)
}

// Inspect answers and THEN parks once, so a test can interleave a Create between
// the answer (NotFound) and the caller acting on it.
func (g *gatedBackend) Inspect(ctx context.Context, id string) (vmlib.HostVM, error) {
	h, err := g.Backend.Inspect(ctx, id)
	if g.inspGate.CompareAndSwap(true, false) {
		g.inspected <- struct{}{}
		<-g.resume
	}
	return h, err
}

func TestGetDuringInFlightCreateNeverReportsNotFound(t *testing.T) {
	g := newGated()
	s, _ := vmlib.NewService(g)
	done := make(chan error, 1)
	go func() {
		_, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "c1", ImageID: "base", Profile: "small"})
		done <- err
	}()
	<-g.entered
	vm, err := s.Get(context.Background(), "c1")
	if err != nil || vm.State != vmlib.StatePreparing {
		t.Fatalf("an unsettled Create must read as preparing, got %+v %v", vm, err)
	}
	close(g.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if vm, err := s.Get(context.Background(), "c1"); err != nil || vm.State != vmlib.StateRunning {
		t.Fatalf("%+v %v", vm, err)
	}
}

func TestDeleteIsASettleBarrierForInFlightCreate(t *testing.T) {
	g := newGated()
	s, _ := vmlib.NewService(g)
	go s.Create(context.Background(), vmlib.CreateRequest{ID: "c2", ImageID: "base", Profile: "small"})
	<-g.entered
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if res, err := s.Delete(short, "c2"); err == nil || res.Deleted {
		t.Fatalf("Delete must not report success before creation settles: %+v %v", res, err)
	}
	delDone := make(chan vmlib.DeleteResult, 1)
	go func() {
		res, _ := s.Delete(context.Background(), "c2")
		delDone <- res
	}()
	time.Sleep(50 * time.Millisecond)
	close(g.proceed)
	if res := <-delDone; !res.Deleted {
		t.Fatalf("barrier Delete must delete the settled VM: %+v", res)
	}
	if g.Count() != 0 {
		t.Fatal("VM left behind")
	}
}

func TestDeleteOfUnknownIDBlocksALateCreate(t *testing.T) {
	g := newGated()
	close(g.proceed)
	s, _ := vmlib.NewService(g)
	if res, err := s.Delete(context.Background(), "late1"); err != nil || !res.Deleted {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "late1", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("a Create arriving after the Delete barrier must be refused, got %v", err)
	}
	if g.Count() != 0 {
		t.Fatal("late Create produced a VM")
	}
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "other", ImageID: "base", Profile: "small"}); err != nil {
		t.Fatalf("unrelated IDs unaffected: %v", err)
	}
}

// Delete's lookup got NotFound and is parked before acting on it; a Create for the
// same ID arriving in that window is refused by the Delete-intent tombstone, so
// the "nothing known" verdict can never be invalidated by a racing Create.
func TestDeleteLookupRaceWithConcurrentCreateIsRefused(t *testing.T) {
	g := newGated()
	g.inspected, g.resume = make(chan struct{}, 1), make(chan struct{})
	g.inspGate.Store(true)
	var once sync.Once
	release := func() { once.Do(func() { close(g.resume); close(g.proceed) }) }
	t.Cleanup(release) // never strand the parked Delete or a gated Prepare
	s, _ := vmlib.NewService(g)
	bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	delDone := make(chan vmlib.DeleteResult, 1)
	go func() {
		res, _ := s.Delete(bg, "r1")
		delDone <- res
	}()
	select {
	case <-g.inspected:
	case <-time.After(5 * time.Second):
		t.Fatal("Delete never reached its lookup")
	}
	if _, err := s.Create(bg, vmlib.CreateRequest{ID: "r1", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("Create racing a Delete must be refused, got %v", err)
	}
	release()
	select {
	case res := <-delDone:
		if !res.Deleted {
			t.Fatalf("%+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Delete never completed")
	}
	if g.Count() != 0 {
		t.Fatal("no VM may exist for the ID")
	}
}

func TestEverySuccessfulDeletePathConsumesTheID(t *testing.T) {
	b := vmlibtest.New(3, "base")
	s, _ := vmlib.NewService(b)
	create := func(id string) error {
		_, err := s.Create(context.Background(), vmlib.CreateRequest{ID: id, ImageID: "base", Profile: "small"})
		return err
	}
	if err := create("d1"); err != nil { // 1. existing VM
		t.Fatal(err)
	}
	if res, err := s.Delete(context.Background(), "d1"); err != nil || !res.Deleted {
		t.Fatal(res, err)
	}
	if res, err := s.Delete(context.Background(), "d1"); err != nil || !res.Deleted { // 2. repeated delete
		t.Fatal(res, err)
	}
	if err := create("d2"); err != nil { // 3. record whose VM the backend lost
		t.Fatal(err)
	}
	b.ExitVM("d2", true)
	b.Release(context.Background(), "d2")
	if res, err := s.Delete(context.Background(), "d2"); err != nil || !res.Deleted {
		t.Fatal(res, err)
	}
	if res, err := s.Delete(context.Background(), "d3"); err != nil || !res.Deleted { // 4. nothing known
		t.Fatal(res, err)
	}
	for _, id := range []string{"d1", "d2", "d3"} {
		if err := create(id); !errors.Is(err, vmlib.ErrAlreadyExists) {
			t.Fatalf("%s: a deleted ID must never come back in this Service (no expiry), got %v", id, err)
		}
	}
	if b.Count() != 0 {
		t.Fatal("late creates produced VMs")
	}
}

func TestAmbiguousPrepareWithNoBackendProofStaysRetainedAndSticky(t *testing.T) {
	g := newGated()
	close(g.proceed)
	g.prepareErr = errors.New("transport broke") // backend never heard of the VM (yet)
	s, _ := vmlib.NewService(g)
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "a1", ImageID: "base", Profile: "small"}); err == nil {
		t.Fatal("expected error")
	}
	vm, err := s.Get(context.Background(), "a1")
	if err != nil || !vm.Retained {
		t.Fatalf("identity must stay visible and retained, got %+v %v", vm, err)
	}
	// Inspect NotFound and a Release reporting "already released" (the delayed
	// Prepare may still land) are NOT proof: Delete must keep the identity.
	for i := 0; i < 2; i++ {
		res, err := s.Delete(context.Background(), "a1")
		if !errors.Is(err, vmlib.ErrRetained) || res.Deleted {
			t.Fatalf("Delete must not clear an unproven ambiguous prepare: %+v %v", res, err)
		}
	}
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "a1", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("the identity stays reserved: %v", err)
	}
}

func TestAmbiguousPrepareThatLandedIsReleasedWithProof(t *testing.T) {
	g := newGated()
	close(g.proceed)
	g.errAfter, g.prepareErr = true, errors.New("reply lost")
	s, _ := vmlib.NewService(g)
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "a2", ImageID: "base", Profile: "small"}); err == nil {
		t.Fatal("expected error")
	}
	if g.Count() != 0 {
		t.Fatal("a VM the backend reports as prepared must be released")
	}
	if _, err := s.Get(context.Background(), "a2"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("settled and released: %v", err)
	}
}

func TestDeleteOfVMTheBackendNoLongerKnows(t *testing.T) {
	b := vmlibtest.New(3, "base")
	s, _ := vmlib.NewService(b)
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "v1", ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	b.ExitVM("v1", true)
	if _, err := b.Release(context.Background(), "v1"); err != nil { // removed behind the Service's back
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "v1"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("settled VM missing on the backend is NotFound: %v", err)
	}
	if res, err := s.Delete(context.Background(), "v1"); err != nil || !res.Deleted {
		t.Fatalf("Delete must clear a settled record the backend lost: %+v %v", res, err)
	}
}

// pauseContext parks the FIRST caller of Done() (Delete's lock wait) until resumed,
// giving a deterministic window in which Delete holds a captured record.
type pauseContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
	resume  chan struct{}
}

func (c *pauseContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered); <-c.resume })
	return nil
}

// ABA: Delete captures the old in-flight record; the old Create drops it and a
// same-ID successor would install a new record before Delete's lock wait ends.
// Delete intent reserves the ID, so the successor is refused.
func TestDeleteIntentRefusesSuccessorCreateABA(t *testing.T) {
	g := newGated()
	g.prepareErr = vmlib.ErrResourceExhausted // the old Create fails definitively and drops its record
	s, _ := vmlib.NewService(g)
	req := vmlib.CreateRequest{ID: "aba", ImageID: "base", Profile: "small"}
	firstDone := make(chan error, 1)
	go func() { _, err := s.Create(context.Background(), req); firstDone <- err }()
	<-g.entered
	ctx := &pauseContext{Context: context.Background(), entered: make(chan struct{}), resume: make(chan struct{})}
	delDone := make(chan vmlib.DeleteResult, 1)
	go func() { res, _ := s.Delete(ctx, "aba"); delDone <- res }()
	<-ctx.entered // Delete holds the old record and is about to wait on its lock
	close(g.proceed)
	if err := <-firstDone; !errors.Is(err, vmlib.ErrResourceExhausted) {
		t.Fatal(err)
	}
	g.prepareErr = nil
	if _, err := s.Create(context.Background(), req); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("a successor Create for an ID under Delete must be refused, got %v", err)
	}
	close(ctx.resume)
	res := <-delDone
	if g.Count() != 0 {
		t.Fatalf("no replacement VM may exist (Deleted=%v)", res.Deleted)
	}
	if _, err := s.Get(context.Background(), "aba"); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("nothing may exist for the ID: %v", err)
	}
}

func TestDeleteIntentSpendsIDEvenWhenDeleteFails(t *testing.T) {
	g := newGated()
	close(g.proceed)
	g.prepareErr = errors.New("transport broke") // unprovable ambiguous prepare: Delete keeps failing
	s, _ := vmlib.NewService(g)
	s.Create(context.Background(), vmlib.CreateRequest{ID: "x1", ImageID: "base", Profile: "small"})
	if res, err := s.Delete(context.Background(), "x1"); err == nil || res.Deleted {
		t.Fatalf("expected retained failure: %+v %v", res, err)
	}
	if _, err := s.Delete(context.Background(), "BAD ID"); !errors.Is(err, vmlib.ErrInvalidArgument) {
		t.Fatalf("malformed IDs are rejected before any reservation: %v", err)
	}
	// a cancelled Delete also spends the ID
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Delete(cctx, "y1")
	if _, err := s.Create(context.Background(), vmlib.CreateRequest{ID: "y1", ImageID: "base", Profile: "small"}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatalf("Delete intent must reserve the ID: %v", err)
	}
}
