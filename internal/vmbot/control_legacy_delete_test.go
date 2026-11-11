package vmbot

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

const legacyVM = "bot-abcdef"
const legacyTok = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// countCreateLC counts backend Create calls.
type countCreateLC struct {
	vmlib.Lifecycle
	creates atomic.Int32
}

func (l *countCreateLC) Create(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
	l.creates.Add(1)
	return l.Lifecycle.Create(ctx, r)
}

func writeLegacy(t *testing.T, dir string, rec VMRecord, offset int64) {
	t.Helper()
	legacy, e := OpenStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = legacy.Update(func(s *State) { s.Generation = rec.Generation; s.Offset = offset; s.VMs = []VMRecord{rec} }); e != nil {
		t.Fatal(e)
	}
	legacy.Close()
}

func startMigrated(t *testing.T, dir string, lc vmlib.Lifecycle) (*Store, *Controller) {
	t.Helper()
	st, e := OpenControlStore(dir, 11)
	if e != nil {
		t.Fatal(e)
	}
	c := NewController(lc, st, &controlTerms{}, ControllerConfig{Profile: "small", OpTimeout: 2 * time.Second})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 2})
	if e = c.Start(context.Background()); e != nil {
		st.Close()
		t.Fatal(e)
	}
	return st, c
}

func remoteVM(t *testing.T, svc *vmlib.Service) {
	t.Helper()
	if _, e := svc.Create(context.Background(), vmlib.CreateRequest{ID: legacyVM, ImageID: "ubuntu", Profile: "small"}); e != nil {
		t.Fatal(e)
	}
}

func deleteOps(s State) []ControlOperation {
	var out []ControlOperation
	for _, o := range s.Operations {
		if o.Kind == "delete" && o.VMID == legacyVM {
			out = append(out, o)
		}
	}
	return out
}

func awaitGone(t *testing.T, svc *vmlib.Service, st *Store) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		_, e := svc.Get(context.Background(), legacyVM)
		if errors.Is(e, vmlib.ErrNotFound) && len(st.Snapshot().VMs) == 0 {
			return
		}
	}
	t.Fatalf("delete never settled: %+v", st.Snapshot())
}

// Legacy intent persisted (deleting, Seq 1) but never enqueued: migration must
// synthesize one stable owned delete operation and recovery must complete it.
func TestControlMigrationSynthesizesDeleteForLegacyIntentBeforeEnqueue(t *testing.T) {
	dir := t.TempDir() + "/state"
	rec := VMRecord{ID: legacyVM, Token: legacyTok, Phase: PhaseDeleting, Generation: 1, Seq: 1, Label: "legacy"}
	writeLegacy(t, dir, rec, 77)
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	remoteVM(t, svc)

	// Synthesis is part of the migration itself, before any engine exists.
	st, e := OpenControlStore(dir, 11)
	if e != nil {
		t.Fatal(e)
	}
	ops := deleteOps(st.Snapshot())
	if len(ops) != 1 || ops[0].OwnerUserID != 11 || ops[0].Status != "pending" || ops[0].Key == "" || len(ops[0].Digest) != 64 {
		t.Fatalf("migration did not synthesize one owned delete op: %+v", st.Snapshot().Operations)
	}
	first := ops[0]
	snap := st.Snapshot()
	if got := snap.VMs[0]; got.ID != legacyVM || got.Token != legacyTok || got.Generation != 1 || got.Seq != 1 || got.OwnerUserID != 11 || got.Phase != PhaseDeleting || got.Label != "legacy" || snap.Offset != 77 {
		t.Fatalf("identity not preserved: %+v offset=%d", got, snap.Offset)
	}
	st.Close()

	// Reopen: no duplicate operation.
	st, c := startMigrated(t, dir, svc)
	if ops = deleteOps(st.Snapshot()); len(ops) != 1 || ops[0].ID != first.ID || ops[0].Key != first.Key {
		t.Fatalf("restart duplicated/changed op: %+v", st.Snapshot().Operations)
	}
	if e = c.RecoverControl(); e != nil {
		t.Fatal(e)
	}
	if e = c.RecoverControl(); e != nil { // repeated recovery is idempotent too
		t.Fatal(e)
	}
	awaitGone(t, svc, st)
	if ops = deleteOps(st.Snapshot()); len(ops) != 1 || ops[0].ID != first.ID || ops[0].Status != "succeeded" {
		t.Fatalf("ops after recovery: %+v", st.Snapshot().Operations)
	}
	// The owner can read the inherited operation.
	if o, e := c.ControlOperation(11, first.ID); e != nil || o.Status != "succeeded" {
		t.Fatalf("%+v %v", o, e)
	}
	c.Close()
	st.Close()
}

// An already-migrated store (older build) with a deleting VM and no operation is
// repaired durably by RecoverControl, once.
func TestControlRecoverRepairsDeletingVMWithoutOperation(t *testing.T) {
	dir := t.TempDir() + "/state"
	st, e := OpenControlStore(dir, 0)
	if e != nil {
		t.Fatal(e)
	}
	st.Update(func(s *State) {
		s.Generation = 1
		s.VMs = []VMRecord{{ID: legacyVM, Token: legacyTok, Phase: PhaseDeleting, Generation: 1, Seq: 3, Label: "legacy", OwnerUserID: 11}}
	})
	st.Close()
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	remoteVM(t, svc)
	st, c := startMigrated(t, dir, svc)
	defer st.Close()
	defer c.Close()
	if e = c.RecoverControl(); e != nil {
		t.Fatal(e)
	}
	if n := len(deleteOps(st.Snapshot())); n != 1 {
		t.Fatalf("ops=%d", n)
	}
	awaitGone(t, svc, st)
	if e = c.RecoverControl(); e != nil || len(deleteOps(st.Snapshot())) != 1 {
		t.Fatalf("second recovery duplicated: %v %+v", e, st.Snapshot().Operations)
	}
}

// A legacy delete workflow queued in the SQLite DB is retired without effect; the
// synthesized control operation performs the single delete.
func TestControlMigrationQueuedLegacyDeleteWorkflowHandoff(t *testing.T) {
	dir := t.TempDir() + "/state"
	writeLegacy(t, dir, VMRecord{ID: legacyVM, Token: legacyTok, Phase: PhaseDeleting, Generation: 1, Seq: 1, Label: "legacy"}, 0)
	base, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	remoteVM(t, base)
	st, c := startMigrated(t, dir, base)
	defer st.Close()
	defer c.Close()
	h, e := c.submit(OpInput{Kind: opDelete, VMID: legacyVM, Seq: 1}, deleteID(legacyVM, 1))
	if e != nil {
		t.Fatal(e)
	}
	r, e := c.wait(h, 3*time.Second)
	if e != nil || r.Msg != "legacy workflow retired by control migration" {
		t.Fatalf("legacy workflow must be retired: %+v %v", r, e)
	}
	if vm, e := base.Get(context.Background(), legacyVM); e != nil || vm.State != vmlib.StateRunning {
		t.Fatalf("legacy workflow had an effect: %+v %v", vm, e)
	}
	if e = c.RecoverControl(); e != nil {
		t.Fatal(e)
	}
	awaitGone(t, base, st)
}

// A legacy creating record cannot be handed to an owner: its Create outcome is
// unknown to this build. Startup is rejected with a diagnostic; the backend is
// never touched and no Create is issued (before or after rejection).
func TestControlMigrationLegacyCreatingNeverIssuesCreate(t *testing.T) {
	for _, present := range []bool{false, true} {
		dir := t.TempDir() + "/state"
		writeLegacy(t, dir, VMRecord{ID: legacyVM, Token: legacyTok, Phase: PhaseCreating, Generation: 1, Label: "legacy"}, 0)
		base, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
		if present {
			remoteVM(t, base)
		}
		lc := &countCreateLC{Lifecycle: base}
		st, e := OpenControlStore(dir, 11)
		if e != nil {
			t.Fatal(e)
		}
		c := NewController(lc, st, &controlTerms{}, ControllerConfig{Profile: "small", OpTimeout: 2 * time.Second})
		c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 2})
		e = c.Start(context.Background())
		if e == nil || !strings.Contains(e.Error(), legacyVM) {
			t.Fatalf("present=%v: start must reject with a diagnostic naming the VM: %v", present, e)
		}
		if lc.creates.Load() != 0 {
			t.Fatalf("present=%v: rejected handoff issued a Create", present)
		}
		if _, e := base.Get(context.Background(), legacyVM); present == errors.Is(e, vmlib.ErrNotFound) {
			t.Fatalf("present=%v: backend state changed (%v)", present, e)
		}
		if len(st.Snapshot().VMs) != 1 {
			t.Fatal("rejected handoff dropped the record")
		}
		st.Close()
	}
}
