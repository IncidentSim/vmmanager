package vmbot

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// fakeTerms records terminal manager calls.
type fakeTerms struct {
	mu      sync.Mutex
	started []TerminalTarget
	revoked []string
	events  *[]string
	failErr error
}

func (f *fakeTerms) Start(_ context.Context, t TerminalTarget) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.started = append(f.started, t)
	return nil
}
func (f *fakeTerms) Revoke(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, id)
	if f.events != nil {
		*f.events = append(*f.events, "revoke:"+id)
	}
}
func (f *fakeTerms) Close() {}

// hookLC wraps a Lifecycle so tests can inject ambiguous failures.
type hookLC struct {
	vmlib.Lifecycle
	create func(ctx context.Context, req vmlib.CreateRequest) (vmlib.VM, error)
	del    func(ctx context.Context, id string) (vmlib.DeleteResult, error)
	get    func(ctx context.Context, id string) (vmlib.VM, error)
	list   func(ctx context.Context) ([]vmlib.VM, error)
}

func (h *hookLC) Create(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
	if h.create != nil {
		return h.create(ctx, r)
	}
	return h.Lifecycle.Create(ctx, r)
}
func (h *hookLC) Delete(ctx context.Context, id string) (vmlib.DeleteResult, error) {
	if h.del != nil {
		return h.del(ctx, id)
	}
	return h.Lifecycle.Delete(ctx, id)
}
func (h *hookLC) Get(ctx context.Context, id string) (vmlib.VM, error) {
	if h.get != nil {
		return h.get(ctx, id)
	}
	return h.Lifecycle.Get(ctx, id)
}
func (h *hookLC) List(ctx context.Context) ([]vmlib.VM, error) {
	if h.list != nil {
		return h.list(ctx)
	}
	return h.Lifecycle.List(ctx)
}

type env struct {
	be    *vmlibtest.Backend
	svc   *vmlib.Service
	lc    *hookLC
	store *Store
	terms *fakeTerms
	ctl   *Controller
	dir   string
	max   int
}

// newEnv is the legacy one-slot environment (application max_vms = 1): the
// ADR 0002 guarantees are asserted against it unchanged. Multi-VM behaviour is
// tested with newEnvMax.
func newEnv(t *testing.T) *env { return newEnvMax(t, 1) }

func newEnvMax(t *testing.T, maxVMs int) *env {
	t.Helper()
	be := vmlibtest.New(3, "base")
	svc, err := vmlib.NewService(be)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{be: be, svc: svc, lc: &hookLC{Lifecycle: svc}, terms: &fakeTerms{}, dir: filepath.Join(t.TempDir(), "state"), max: maxVMs}
	e.store, err = OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.ctl.Close(); e.store.Close() })
	e.ctl = e.newController(t)
	return e
}

// testLogWriter routes controller logs into the test log (safe after the test ends).
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	defer func() { _ = recover() }() // t.Log after test completion panics
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// newController builds AND starts a controller: the real DBOS layer on a real
// SQLite file under the state directory (there is no non-durable mode).
func (e *env) newController(t *testing.T) *Controller {
	t.Helper()
	c := NewController(e.lc, e.store, e.terms, ControllerConfig{
		ImageID: "base", Profile: "small", PublicOrigin: "https://bot.example.test:8443",
		MaxVMs: e.max, OpTimeout: 2 * time.Second, QueryTimeout: time.Second,
		Logger: log.New(testLogWriter{t}, "ctl: ", 0),
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

// restart is a graceful process restart: orchestration stops, the state lock is
// released, everything reopens on the same directories.
func (e *env) restart(t *testing.T) {
	t.Helper()
	e.ctl.Close()
	e.store.Close()
	var err error
	e.store, err = OpenStore(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.terms = &fakeTerms{}
	e.ctl = e.newController(t)
}

// vm is the single owned record, or nil when there is none.
func (e *env) vm() *VMRecord {
	vms := e.store.Snapshot().VMs
	if len(vms) == 0 {
		return nil
	}
	v := vms[0]
	return &v
}

// only returns the single owned record.
func (e *env) only(t *testing.T) VMRecord {
	t.Helper()
	vms := e.store.Snapshot().VMs
	if len(vms) != 1 {
		t.Fatalf("want exactly one record, have %d", len(vms))
	}
	return vms[0]
}

func TestCreateSavesIdentityAndCapabilityBeforeRemoteCall(t *testing.T) {
	e := newEnv(t)
	var sawPhase string
	var sawID, sawTok string
	e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		v := e.only(t)
		sawPhase, sawID, sawTok = v.Phase, v.ID, v.Token
		if r.ID != v.ID {
			t.Errorf("request ID %s != persisted %s", r.ID, v.ID)
		}
		return e.svc.Create(ctx, r)
	}
	st, err := e.ctl.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if sawPhase != PhaseCreating || sawID == "" || sawTok == "" {
		t.Fatalf("not persisted before create: %q %q %q", sawPhase, sawID, sawTok)
	}
	if !st.Present || st.Phase != PhaseActive || st.Generation != 1 {
		t.Fatalf("status %+v", st)
	}
	if len(e.terms.started) != 1 || e.terms.started[0].VMID != st.VMID || e.terms.started[0].Token != sawTok || e.terms.started[0].Host == "" {
		t.Fatalf("terminal start: %+v", e.terms.started)
	}
}

func TestTokenIs256BitAndRotatesPerVM(t *testing.T) {
	e := newEnv(t)
	st1, _ := e.ctl.Create(context.Background(), "")
	tok1 := e.only(t).Token
	raw, err := base64.RawURLEncoding.DecodeString(tok1)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token must be 32 random bytes base64url: %v %d", err, len(raw))
	}
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err != nil {
		t.Fatal(err)
	}
	st2, err := e.ctl.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	tok2 := e.only(t).Token
	if tok1 == tok2 || st1.VMID == st2.VMID || st2.Generation != 2 {
		t.Fatalf("replacement must rotate ID/token/generation: %v %v %d", tok1 == tok2, st1.VMID == st2.VMID, st2.Generation)
	}
	link, err := e.ctl.Terminal(context.Background(), Ref{})
	if err != nil || link != "https://bot.example.test:8443/t/"+tok2+"/" {
		t.Fatalf("link %q %v", link, err)
	}
	if strings.Contains(link, tok1) {
		t.Fatal("stale token leaked into new link")
	}
}

func TestConcurrentCreatesAreSerializedToOneVM(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, occupied int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.ctl.Create(context.Background(), "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrCapacity):
				occupied++
			default:
				t.Errorf("unexpected %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || occupied != 7 || e.be.Count() != 1 || e.be.Prepared != 1 {
		t.Fatalf("ok=%d occupied=%d vms=%d prepared=%d", ok, occupied, e.be.Count(), e.be.Prepared)
	}
}

// Foreign VMs are never adopted, listed as ours or deleted. They consume backend
// capacity, and the backend's own admission stays authoritative.
func TestForeignVMsAreNeverTouchedAndBackendCapacityIsAuthoritative(t *testing.T) {
	e := newEnvMax(t, 8)
	var foreign []string
	for i := 0; i < 2; i++ {
		v, err := e.svc.Create(context.Background(), vmlib.CreateRequest{ImageID: "base", Profile: "small"})
		if err != nil {
			t.Fatal(err)
		}
		foreign = append(foreign, v.ID)
	}
	if _, err := e.ctl.Delete(context.Background(), Ref{}); !errors.Is(err, ErrNoVM) {
		t.Fatalf("delete must not touch foreign VMs: %v", err)
	}
	if _, err := e.ctl.Create(context.Background(), "mine"); err != nil {
		t.Fatalf("a foreign VM below backend capacity must not block create: %v", err)
	}
	// Backend is now full (capacity 3): its refusal is reported, nothing is left owned.
	_, err := e.ctl.Create(context.Background(), "extra")
	if !errors.Is(err, ErrBackendFull) {
		t.Fatalf("want ErrBackendFull, got %v", err)
	}
	if n := len(e.store.Snapshot().VMs); n != 1 {
		t.Fatalf("a definitively refused create must not stay owned: %d records", n)
	}
	for _, id := range foreign {
		if v, err := e.svc.Get(context.Background(), id); err != nil || v.State != vmlib.StateRunning {
			t.Fatalf("foreign VM disturbed: %v %v", v, err)
		}
	}
	list, _ := e.ctl.List(context.Background())
	if len(list) != 1 || list[0].Name != "mine" {
		t.Fatalf("only owned VMs are listed: %+v", list)
	}
}

func TestCreateDoesNotDependOnListing(t *testing.T) {
	e := newEnv(t)
	e.lc.list = func(context.Context) ([]vmlib.VM, error) { return nil, errors.New("rpc: unavailable /var/secret/path") }
	if _, err := e.ctl.Create(context.Background(), ""); err != nil {
		t.Fatalf("create must not call List: %v", err)
	}
}

func TestAmbiguousCreateKeepsOccupancyAndAdoptsVM(t *testing.T) {
	e := newEnv(t)
	e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		if _, err := e.svc.Create(ctx, r); err != nil { // VM really exists
			return vmlib.VM{}, err
		}
		return vmlib.VM{}, context.DeadlineExceeded // response lost
	}
	st, err := e.ctl.Create(context.Background(), "")
	if err == nil || !st.Present {
		t.Fatalf("ambiguous failure must keep the VM occupied: %+v %v", st, err)
	}
	if _, err := e.ctl.Create(context.Background(), ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("second create must be refused: %v", err)
	}
	s2, err := e.ctl.Status(context.Background(), Ref{})
	if err != nil || !s2.Present || s2.State != string(vmlib.StateRunning) {
		t.Fatalf("status after adoption: %+v %v", s2, err)
	}
	if e.be.Count() != 1 {
		t.Fatal("exactly one VM expected")
	}
}

func TestAmbiguousCreateThenRemoteUnreachableStaysOccupied(t *testing.T) {
	e := newEnv(t)
	e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
		e.svc.Create(ctx, r)
		e.lc.get = func(context.Context, string) (vmlib.VM, error) { return vmlib.VM{}, errors.New("unavailable") }
		return vmlib.VM{}, context.DeadlineExceeded
	}
	st, err := e.ctl.Create(context.Background(), "")
	if err == nil || !st.Present || !st.Uncertain {
		t.Fatalf("want uncertain occupied, got %+v %v", st, err)
	}
	e.lc.list = func(context.Context) ([]vmlib.VM, error) { return nil, errors.New("unavailable") }
	if _, err := e.ctl.Create(context.Background(), ""); err == nil {
		t.Fatal("must not create a second VM while the first is unresolved")
	}
	e.lc.get, e.lc.list = nil, nil
	if err := e.ctl.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := e.vm(); v == nil || v.Phase != PhaseActive {
		t.Fatalf("recovered state %+v", v)
	}
}

func TestDefinitiveCreateFailureReleasesOccupancy(t *testing.T) {
	e := newEnv(t)
	e.be.StartErr = errors.New("boom")
	if _, err := e.ctl.Create(context.Background(), ""); err == nil {
		t.Fatal("expected failure")
	}
	if e.vm() != nil || e.be.Count() != 0 {
		t.Fatalf("rolled-back create must free the slot: %+v count=%d", e.vm(), e.be.Count())
	}
	e.be.StartErr = nil
	if _, err := e.ctl.Create(context.Background(), ""); err != nil {
		t.Fatalf("create after rollback: %v", err)
	}
}

func TestRetainedCreateFailureStaysOccupied(t *testing.T) {
	e := newEnv(t)
	e.be.StartUncertainErr = errors.New("uncertain start")
	st, err := e.ctl.Create(context.Background(), "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !st.Present {
		t.Fatalf("a VM that may be running must stay occupied: %+v", st)
	}
	if _, err := e.ctl.Create(context.Background(), ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteRevokesTerminalBeforeRemoteDelete(t *testing.T) {
	e := newEnv(t)
	var events []string
	e.terms.events = &events
	st, _ := e.ctl.Create(context.Background(), "")
	e.lc.del = func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
		events = append(events, "delete:"+id)
		return e.svc.Delete(ctx, id)
	}
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0] != "revoke:"+st.VMID {
		t.Fatalf("order %v", events)
	}
	deletes := 0
	for _, event := range events {
		if event == "delete:"+st.VMID {
			deletes++
		} else if event != "revoke:"+st.VMID {
			t.Fatalf("unexpected event: %v", events)
		}
	}
	if deletes != 1 {
		t.Fatalf("want one delete, preceded by revocation: %v", events)
	}
	if e.vm() != nil || e.be.Count() != 0 {
		t.Fatal("deleted VM must free the application slot")
	}
}

func TestUnknownDeleteResultKeepsOccupancyAndAllowsRetry(t *testing.T) {
	e := newEnv(t)
	st, _ := e.ctl.Create(context.Background(), "")
	fail := true
	e.lc.del = func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
		if fail {
			return vmlib.DeleteResult{}, context.DeadlineExceeded // request never reached the daemon
		}
		return e.svc.Delete(ctx, id)
	}
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err == nil {
		t.Fatal("expected error")
	}
	if v := e.vm(); v == nil || v.Phase != PhaseDeleting || v.ID != st.VMID {
		t.Fatalf("must stay occupied while deletion is unconfirmed: %+v", v)
	}
	if _, err := e.ctl.Create(context.Background(), ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("create during unconfirmed delete: %v", err)
	}
	if _, err := e.ctl.Terminal(context.Background(), Ref{}); err == nil {
		t.Fatal("terminal must stay revoked after a delete attempt")
	}
	fail = false
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if e.vm() != nil {
		t.Fatal("retry must release")
	}
	if _, err := e.ctl.Create(context.Background(), ""); err != nil {
		t.Fatalf("create after confirmed delete: %v", err)
	}
}

func TestRetainedDeleteKeepsVMUntilExitProven(t *testing.T) {
	e := newEnv(t)
	e.be.AutoExit = false
	st, err := e.ctl.Create(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	e.ctl.cfg.OpTimeout = 150 * time.Millisecond
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err == nil {
		t.Fatal("delete must fail while exit is unproven")
	}
	if e.vm() == nil {
		t.Fatal("retained VM must never be abandoned")
	}
	e.be.ExitVM(st.VMID, true)
	e.ctl.cfg.OpTimeout = 2 * time.Second
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err != nil {
		t.Fatalf("retry after exit: %v", err)
	}
}

func TestDeleteIsBoundToGeneration(t *testing.T) {
	e := newEnv(t)
	e.ctl.Create(context.Background(), "")
	e.ctl.Delete(context.Background(), Ref{})
	st2, _ := e.ctl.Create(context.Background(), "")
	if _, err := e.ctl.Delete(context.Background(), Ref{Gen: 1}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale generation must be refused: %v", err)
	}
	if _, err := e.svc.Get(context.Background(), st2.VMID); err != nil {
		t.Fatalf("replacement VM must be untouched: %v", err)
	}
	if _, err := e.ctl.Delete(context.Background(), Ref{Gen: 2}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartPreservesLinkAndRestartsTerminal(t *testing.T) {
	e := newEnv(t)
	st, _ := e.ctl.Create(context.Background(), "")
	link1, _ := e.ctl.Terminal(context.Background(), Ref{})
	e.restart(t)
	if _, err := e.ctl.Create(context.Background(), ""); !errors.Is(err, ErrCapacity) {
		t.Fatalf("restart must not allow a second VM: %v", err)
	}
	if err := e.ctl.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	link2, err := e.ctl.Terminal(context.Background(), Ref{})
	if err != nil || link1 != link2 {
		t.Fatalf("link must survive restart: %q %q %v", link1, link2, err)
	}
	if len(e.terms.started) == 0 || e.terms.started[0].VMID != st.VMID {
		t.Fatalf("terminal not restarted: %+v", e.terms.started)
	}
	if e.be.Count() != 1 {
		t.Fatal("restart must never delete the VM")
	}
}

func TestRecoverClearsVMGoneAndKeepsOnUnknown(t *testing.T) {
	e := newEnv(t)
	st, _ := e.ctl.Create(context.Background(), "")
	e.restart(t)
	e.lc.get = func(context.Context, string) (vmlib.VM, error) { return vmlib.VM{}, errors.New("timeout") }
	if err := e.ctl.Recover(context.Background()); err == nil {
		t.Fatal("expected uncertain recovery error")
	}
	if e.vm() == nil {
		t.Fatal("unknown remote state must keep occupancy")
	}
	e.lc.get = nil
	e.svc.Delete(context.Background(), st.VMID) // removed out of band
	if err := e.ctl.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.vm() != nil {
		t.Fatal("definitively gone VM must release occupancy")
	}
	if len(e.terms.revoked) == 0 {
		t.Fatal("terminal must be revoked for a VM that no longer exists")
	}
}

func TestTerminalRequiresRunningVM(t *testing.T) {
	e := newEnv(t)
	if _, err := e.ctl.Terminal(context.Background(), Ref{}); !errors.Is(err, ErrNoVM) {
		t.Fatalf("got %v", err)
	}
	st, _ := e.ctl.Create(context.Background(), "")
	e.be.ExitVM(st.VMID, true)
	if _, err := e.ctl.Terminal(context.Background(), Ref{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped VM: %v", err)
	}
}

func TestErrorsDoNotLeakRPCInternals(t *testing.T) {
	e := newEnv(t)
	e.lc.create = func(context.Context, vmlib.CreateRequest) (vmlib.VM, error) {
		return vmlib.VM{}, errors.New("rpc error: /srv/firecracker/secret.sock denied")
	}
	e.lc.get = func(context.Context, string) (vmlib.VM, error) { return vmlib.VM{}, vmlib.ErrNotFound }
	_, err := e.ctl.Create(context.Background(), "")
	if err == nil || strings.Contains(err.Error(), "/srv/firecracker") {
		t.Fatalf("leak: %v", err)
	}
}

func TestDeleteLostResponseIsResolvedOnlyByTheDeleteBarrier(t *testing.T) {
	e := newEnv(t)
	e.ctl.Create(context.Background(), "")
	e.lc.del = func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
		e.svc.Delete(ctx, id) // really deleted, response lost
		return vmlib.DeleteResult{}, context.DeadlineExceeded
	}
	if _, err := e.ctl.Delete(context.Background(), Ref{}); err == nil {
		t.Fatal("a lost response must not by itself release the slot (Get NotFound is not proof)")
	}
	if v := e.vm(); v == nil || v.Phase != PhaseDeleting {
		t.Fatalf("slot must stay occupied in deleting phase: %+v", v)
	}
	e.lc.del = nil // the retry (or next reconcile) reaches vmlib's idempotent Delete
	if _, err := e.ctl.Status(context.Background(), Ref{}); err != nil {
		t.Fatal(err)
	}
	if e.vm() != nil || e.be.Count() != 0 {
		t.Fatal("successful Delete barrier must release the slot")
	}
}
