package vmbot

import (
	"context"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// An unexpected failure of the Create child (here a panic inside the effect,
// before or after the VM came into existence) is an operation outcome: the
// operation fails, the record is retained, the owner stays usable and nothing
// recreates the VM. An explicit Delete then settles it.
func TestControlChildFailureIsOutcomeNotRepair(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[exists], func(t *testing.T) {
			t.Parallel()
			e := newOwnerEnv(t)
			creates := 0
			e.ctl.lc = &hookLC{Lifecycle: e.svc, create: func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
				creates++
				if exists {
					e.svc.Create(ctx, r)
				}
				panic("injected durable workflow failure")
			}}
			o := e.accept("create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
			got := awaitControl(t, e.ctl, ownerUser, o.ID)
			if got.Status != "failed" {
				t.Fatalf("want failed outcome, got %+v", got)
			}
			if st := e.ownerStatus(o.VMID); st != "PENDING" {
				t.Fatalf("owner must survive a child failure, status %s", st)
			}
			if _, err := e.ctl.ControlVM(ownerUser, o.VMID); err != nil {
				t.Fatal("record must be retained")
			}
			if err := e.ctl.RecoverControl(); err != nil || creates != 1 {
				t.Fatalf("recovery recreated or failed: %v creates=%d", err, creates)
			}
			d := e.accept("delete", ControlRequest{Kind: "delete", VMID: o.VMID})
			if got := awaitControl(t, e.ctl, ownerUser, d.ID); got.Status != "succeeded" {
				t.Fatalf("explicit delete %+v", got)
			}
			if len(e.st.Snapshot().VMs) != 0 {
				t.Fatal("record not released after confirmed delete")
			}
		})
	}
}
