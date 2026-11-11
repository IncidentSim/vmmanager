package vmbot

import (
	"context"
	"errors"
	"log"
	"os"
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
}

// newEnv builds a non-control store, a started engine and in-memory backend.
func newEnv(t *testing.T) *env {
	t.Helper()
	be := vmlibtest.New(3, "base")
	svc, err := vmlib.NewService(be)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{be: be, svc: svc, lc: &hookLC{Lifecycle: svc}, terms: &fakeTerms{}, dir: filepath.Join(t.TempDir(), "state")}
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
		Profile: "small", PublicOrigin: "https://bot.example.test:8443",
		OpTimeout: 2 * time.Second, QueryTimeout: time.Second,
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

// seedActive creates the backend VM and stores an active record for it, the
// state an owner commit would leave behind.
func (e *env) seedActive(t *testing.T, label string) VMRecord {
	t.Helper()
	rec := VMRecord{ID: "bot-seed" + label, Token: testTok(len(label) + 7), Phase: PhaseActive, Generation: 1, Label: label}
	if _, err := e.svc.Create(context.Background(), vmlib.CreateRequest{ID: rec.ID, ImageID: "base", Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Update(func(s *State) { s.Generation = 1; s.VMs = []VMRecord{rec} }); err != nil {
		t.Fatal(err)
	}
	return rec
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

// A deliberately separate, file-backed remote-resource stand-in. It survives
// the controller process and its SQLite checkpoints independently.
type diskLC struct {
	vmlib.Lifecycle
	root, mode string
}

func (d *diskLC) path(id string) string { return filepath.Join(d.root, "remote", id) }
func (d *diskLC) marker(name string) {
	if err := os.WriteFile(filepath.Join(d.root, name), []byte("ready"), 0600); err != nil {
		panic(err)
	}
}
func (d *diskLC) Create(_ context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
	f, err := os.OpenFile(d.path(r.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return vmlib.VM{}, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return vmlib.VM{}, err
	}
	f.Close()
	_ = syncDir(filepath.Dir(d.path(r.ID)))
	f, err = os.OpenFile(filepath.Join(d.root, "creates"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return vmlib.VM{}, err
	}
	_, err = f.WriteString(r.ID + "\n")
	_ = f.Sync()
	f.Close()
	if err != nil {
		return vmlib.VM{}, err
	}
	if d.mode == "create-effect" || d.mode == "create-delete-intent" {
		d.marker("effect")
		select {}
	}
	return vmlib.VM{ID: r.ID, State: vmlib.StateRunning}, nil
}
func (d *diskLC) Get(_ context.Context, id string) (vmlib.VM, error) {
	_, err := os.Stat(d.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return vmlib.VM{}, vmlib.ErrNotFound
	}
	return vmlib.VM{ID: id, State: vmlib.StateRunning}, err
}
func (d *diskLC) Delete(_ context.Context, id string) (vmlib.DeleteResult, error) {
	err := os.Remove(d.path(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return vmlib.DeleteResult{}, err
	}
	_ = syncDir(filepath.Dir(d.path(id)))
	if d.mode == "delete-effect" {
		d.marker("effect")
		select {}
	}
	return vmlib.DeleteResult{Deleted: true}, nil
}
func (d *diskLC) Stop(_ context.Context, id string) (vmlib.VM, error) {
	f, err := os.OpenFile(filepath.Join(d.root, "stops"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return vmlib.VM{}, err
	}
	_, err = f.WriteString(id + "\n")
	_ = f.Sync()
	f.Close()
	if err != nil {
		return vmlib.VM{}, err
	}
	if d.mode == "stop-effect" {
		d.marker("effect") // effect done, checkpoint not yet written
		select {}
	}
	return vmlib.VM{ID: id, State: vmlib.StateStopped}, nil
}
func (d *diskLC) SSHInfo(context.Context, string) (vmlib.SSHInfo, error) {
	return vmlib.SSHInfo{Host: "127.0.0.1", User: "root"}, nil
}

type diskTerms struct{ lc *diskLC }

func (d diskTerms) Start(context.Context, TerminalTarget) error { d.lc.marker("granted"); return nil }
func (d diskTerms) Revoke(string) {
	if d.lc.mode == "delete-intent" || d.lc.mode == "create-delete-intent" {
		d.lc.marker("intent")
		select {}
	}
}
func (d diskTerms) Close() {}

func waitMarker(ctx context.Context, path string) error {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
