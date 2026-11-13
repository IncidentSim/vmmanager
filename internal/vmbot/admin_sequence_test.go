package vmbot

import (
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
)

func TestAdminSnapshotDeletesSequentiallyAcrossOwners(t *testing.T) {
	e := newOwnerEnv(t)
	first := e.create("one")
	second, err := e.ctl.AcceptControl(22, "two", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if op := awaitControl(t, e.ctl, 22, second.ID); op.Status != "succeeded" {
		t.Fatalf("create: %+v", op)
	}
	release := e.lc.arm("delete")
	defer release()
	e.ctl.ConfigureAdmins([]int64{99})
	b, err := e.ctl.AdminPreview(99, 99, "preview")
	if err != nil {
		t.Fatal(err)
	}
	b, err = e.ctl.AdminAdvance(99, 99, b.Token)
	if err != nil {
		t.Fatal(err)
	}
	e.lc.waitEntered(t, "delete")
	// Repeated callbacks while the first owner is blocked must neither admit nor
	// execute the other owner's deletion.
	for i := 0; i < 3; i++ {
		b, err = e.ctl.AdminAdvance(99, 99, b.Token)
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := e.lc.count("delete:"); n != 1 {
		t.Fatalf("parallel backend deletes: %d", n)
	}
	deletes := 0
	for _, op := range e.st.Snapshot().Operations {
		if op.Kind == "delete" {
			deletes++
			if op.VMID != first.VMID {
				t.Fatal("admitted second ID before first settled")
			}
		}
	}
	if deletes != 1 {
		t.Fatalf("parallel admissions: %d", deletes)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err = e.ctl.AdminAdvance(99, 99, b.Token)
		if err != nil {
			t.Fatal(err)
		}
		if b.Pending == 0 {
			break
		}
		time.Sleep(time.Millisecond * 5)
	}
	if b.Completed != 2 || b.Failed != 0 || b.Pending != 0 || e.lc.count("delete:") != 2 {
		t.Fatalf("result: %+v", b)
	}
	if e.lc.count("deleteimage:") != 0 {
		t.Fatal("deleted images")
	}
	for _, ref := range b.VMs {
		if _, ok := e.record(ref.ID); ok {
			t.Fatal("successful delete retained VM")
		}
	}
}

// The operation is read as pending, then the owner finishes (settling the
// operation first) before the owner state is read. That stale split read must
// reconcile to the authoritative success, never to a failure.
func TestAdminAdvanceStaleOperationReadOwnerSucceeded(t *testing.T) {
	e := newOwnerEnv(t)
	vm := e.create("one")
	e.ctl.ConfigureAdmins([]int64{99})
	b, err := e.ctl.AdminPreview(99, 99, "preview")
	if err != nil {
		t.Fatal(err)
	}
	e.ctl.testAfterAdminOpRead = func() {
		e.ctl.testAfterAdminOpRead = nil
		e.eventually("owner ends", func() bool { return e.ownerStatus(vm.VMID) == dbos.WorkflowStatusSuccess })
	}
	b, err = e.ctl.AdminAdvance(99, 99, b.Token)
	if err != nil {
		t.Fatal(err)
	}
	if b.Failed != 0 || b.VMs[0].ErrorCode != "" {
		t.Fatalf("false failure from stale read: %+v", b)
	}
	b, err = e.ctl.AdminAdvance(99, 99, b.Token)
	if err != nil || b.Completed != 1 || b.Failed != 0 || b.Pending != 0 {
		t.Fatalf("not completed: %+v %v", b, err)
	}
	if got := e.st.Snapshot().AdminBatches[0]; got.Completed != 1 || got.Failed != 0 {
		t.Fatalf("persisted: %+v", got)
	}
}
