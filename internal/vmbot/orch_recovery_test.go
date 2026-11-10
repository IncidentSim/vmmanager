package vmbot

import (
	"context"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"sync/atomic"
	"testing"
)

func TestOrchestrationTerminalCreateErrorReconcilesRemoteOutcome(t *testing.T) {
	for _, exists := range []bool{false, true} {
		name := "before-effect"
		if exists {
			name = "after-effect"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			var creates, barriers atomic.Int32
			e.lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
				creates.Add(1)
				if exists {
					if _, err := e.svc.Create(ctx, r); err != nil {
						panic(err)
					}
				}
				panic("injected workflow failure")
			}
			if _, err := e.ctl.Create(context.Background(), "a"); err == nil {
				t.Fatal("expected workflow error")
			}
			rec := e.only(t)
			_, wf := e.ctl.lookup(createID(rec.ID))
			if wf == nil || wf.Status != dbos.WorkflowStatusError {
				t.Fatal("SDK must actually persist terminal ERROR")
			}
			e.lc.create = func(context.Context, vmlib.CreateRequest) (vmlib.VM, error) {
				creates.Add(1)
				return vmlib.VM{}, vmlib.ErrAlreadyExists
			}
			e.lc.del = func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
				barriers.Add(1)
				return e.svc.Delete(ctx, id)
			}
			e.restart(t)
			if err := e.ctl.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if creates.Load() != 1 {
				t.Fatal("terminal workflow error must not resubmit Create")
			}
			if exists {
				cur, ok := e.store.Snapshot().ByID(rec.ID)
				if !ok || cur.Phase != PhaseActive || cur.Token != rec.Token || cur.Generation != rec.Generation {
					t.Fatal("existing remote VM not adopted with unchanged identity")
				}
				s, err := e.ctl.Status(context.Background(), Ref{Sel: "a"})
				if err != nil || s.Busy || s.Uncertain || !s.Present {
					t.Fatalf("status: %+v %v", s, err)
				}
				if len(e.terms.started) == 0 || barriers.Load() != 0 {
					t.Fatal("existing VM must regain terminal, never be deleted")
				}
			} else if len(e.store.Snapshot().VMs) != 0 || e.be.Count() != 0 || barriers.Load() == 0 {
				t.Fatal("absent VM must release occupancy only after successful deletion barrier")
			}
		})
	}
}
