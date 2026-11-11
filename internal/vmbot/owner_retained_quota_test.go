package vmbot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Current owner-path replacement for the removed legacy assertions "ambiguous
// Create keeps the slot", "unconfirmed Delete keeps the slot" and "restart never
// repeats Create": ambiguous Create (retained) -> application quota consumed ->
// unsuccessful Delete (ErrRetained, timeout, Deleted:false with nil error) ->
// restart -> explicit successful Delete frees the quota. Real Service/fake
// backend, real controller, owner workflows and DBOS SQLite.
func TestOwnerAmbiguousCreateRetainedQuotaSurvivesFailedDeletesAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     vmlib.VMState // state Get reports for the ambiguous VM
		wantPhase string
	}{
		{"running-promoted-active", vmlib.StateRunning, PhaseActive},
		{"preparing-not-promoted", vmlib.StatePreparing, PhaseCreating},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newOwnerEnv(t)

			var mu sync.Mutex
			creates := map[string]int{}
			var ambiguous string
			var delMode atomic.Value // "retained" | "timeout" | "notdeleted" | "ok"
			delMode.Store("ok")
			var deletes atomic.Int32
			hook := &hookLC{Lifecycle: e.svc}
			hook.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
				mu.Lock()
				creates[r.ID]++
				first := ambiguous == "" && creates[r.ID] == 1
				if first {
					ambiguous = r.ID
				}
				mu.Unlock()
				vm, err := e.svc.Create(ctx, r)
				if first && err == nil {
					return vmlib.VM{}, fmt.Errorf("response lost: %w", vmlib.ErrRetained)
				}
				return vm, err
			}
			hook.get = func(ctx context.Context, id string) (vmlib.VM, error) {
				vm, err := e.svc.Get(ctx, id)
				mu.Lock()
				amb := ambiguous
				mu.Unlock()
				if err == nil && id == amb {
					vm.State = tc.state
				}
				return vm, err
			}
			hook.del = func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
				deletes.Add(1)
				switch delMode.Load().(string) {
				case "retained":
					return vmlib.DeleteResult{Retained: true, Detail: "exit not proven"}, fmt.Errorf("%w: test", vmlib.ErrRetained)
				case "timeout":
					return vmlib.DeleteResult{}, context.DeadlineExceeded
				case "notdeleted":
					return vmlib.DeleteResult{Deleted: false}, nil
				}
				return e.svc.Delete(ctx, id)
			}
			install := func() { e.ctl.lc = hook }
			install()

			// 1. Ambiguous Create: failed operation, record and identity retained.
			c := e.accept("amb", ControlRequest{Kind: "create", ImageID: "ubuntu"})
			if got := awaitControl(t, e.ctl, ownerUser, c.ID); got.Status != "failed" {
				t.Fatalf("ambiguous create must be a failed outcome: %+v", got)
			}
			id := c.VMID
			orig, ok := e.st.Snapshot().ByID(id)
			if !ok {
				t.Fatal("record dropped after ambiguous create")
			}
			if orig.Token == "" || orig.Generation == 0 || orig.OwnerUserID != ownerUser {
				t.Fatalf("identity not persisted: %+v", orig)
			}
			if orig.Phase != tc.wantPhase {
				t.Fatalf("phase %q, want %q (pending/preparing must not be promoted)", orig.Phase, tc.wantPhase)
			}
			same := func(stage string) {
				t.Helper()
				cur, ok := e.st.Snapshot().ByID(id)
				if !ok {
					t.Fatalf("%s: record freed", stage)
				}
				if cur.ID != orig.ID || cur.Token != orig.Token || cur.Generation != orig.Generation || cur.OwnerUserID != orig.OwnerUserID {
					t.Fatalf("%s: identity changed: %+v -> %+v", stage, orig, cur)
				}
				if tc.wantPhase == PhaseCreating && cur.Phase == PhaseActive {
					t.Fatalf("%s: preparing VM promoted active", stage)
				}
				if _, err := e.svc.Get(context.Background(), id); err != nil {
					t.Fatalf("%s: backend VM gone: %v", stage, err)
				}
			}
			e.accept("fill1", ControlRequest{Kind: "create", ImageID: "ubuntu"})
			e.accept("fill2", ControlRequest{Kind: "create", ImageID: "ubuntu"})
			e.eventually("fills settle", func() bool {
				n := 0
				for _, v := range e.st.Snapshot().VMs {
					if v.Phase == PhaseActive {
						n++
					}
				}
				return n >= 2
			})
			full := func(stage string) {
				t.Helper()
				if _, err := e.ctl.AcceptControl(ownerUser, "over-"+stage, ControlRequest{Kind: "create", ImageID: "ubuntu"}); !errors.Is(err, ErrCapacity) {
					t.Fatalf("%s: quota not held: %v", stage, err)
				}
			}
			full("after-create")

			// 2. Each unsuccessful Delete keeps record, identity and quota.
			for i, mode := range []string{"retained", "timeout", "notdeleted"} {
				delMode.Store(mode)
				d := e.accept("del-"+mode, ControlRequest{Kind: "delete", VMID: id})
				if got := awaitControl(t, e.ctl, ownerUser, d.ID); got.Status != "failed" {
					t.Fatalf("%s: delete must fail: %+v", mode, got)
				}
				if int(deletes.Load()) != i+1 {
					t.Fatalf("%s: backend deletes=%d", mode, deletes.Load())
				}
				same(mode)
				if p := e.phase(id); p == PhaseActive {
					t.Fatalf("%s: failed delete left VM active (access fence lost)", mode)
				}
				full(mode)
			}

			// 3. Restart: nothing is repeated or freed.
			e.close()
			e.open(false)
			e.ctl.lc = hook
			if err := e.ctl.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			_ = e.ctl.RecoverControl() // may report the unfinished delete; must not act on it
			same("restart")
			full("restart")
			mu.Lock()
			if creates[id] != 1 {
				mu.Unlock()
				t.Fatalf("restart repeated Create: %d", creates[id])
			}
			mu.Unlock()
			if int(deletes.Load()) != 3 {
				t.Fatalf("restart re-ran a delete: %d", deletes.Load())
			}

			// 4. Explicit successful Delete frees record, backend VM and quota.
			delMode.Store("ok")
			d := e.accept("del-ok", ControlRequest{Kind: "delete", VMID: id})
			if got := awaitControl(t, e.ctl, ownerUser, d.ID); got.Status != "succeeded" {
				t.Fatalf("explicit delete: %+v", got)
			}
			if _, ok := e.st.Snapshot().ByID(id); ok {
				t.Fatal("record not freed")
			}
			if _, err := e.svc.Get(context.Background(), id); !errors.Is(err, vmlib.ErrNotFound) {
				t.Fatalf("backend VM remains: %v", err)
			}
			again := e.accept("after-free", ControlRequest{Kind: "create", ImageID: "ubuntu"})
			if got := awaitControl(t, e.ctl, ownerUser, again.ID); got.Status != "succeeded" {
				t.Fatalf("quota not freed: %+v", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if creates[id] != 1 {
				t.Fatalf("Create repeated for %s: %d", id, creates[id])
			}
			// Other VMs' successful creates may start their own terminals; the
			// retained VM and its capability never get one.
			e.terms.mu.Lock()
			defer e.terms.mu.Unlock()
			for _, g := range e.terms.started {
				if g.VMID == id || g.Token == orig.Token {
					t.Fatalf("terminal granted for the retained VM: %+v", g)
				}
			}
		})
	}
}
