package vmbot

import "testing"

// Reproduce the split observation in AdminAdvance: its operation snapshot is
// pending, but the owner has projected the result and exited before the status
// query returns. No timing or scheduling assumptions are needed.
func TestAdminOperationReconcilesCompletedOwner(t *testing.T) {
	e := newOwnerEnv(t)
	created := e.create("seed")
	release := e.lc.arm("delete")
	defer release()
	pending := e.accept("delete", ControlRequest{Kind: "delete", VMID: created.VMID})
	e.lc.waitEntered(t, "delete")
	if pending.Status != "pending" && pending.Status != "running" {
		t.Fatalf("expected unfinished snapshot: %+v", pending)
	}
	release()
	if done := awaitControl(t, e.ctl, ownerUser, pending.ID); done.Status != "succeeded" {
		t.Fatalf("delete: %+v", done)
	}
	e.eventually("owner success", func() bool { return string(e.ownerStatus(created.VMID)) == "SUCCESS" })
	if got := e.ctl.adminOperation(pending); got.Status != "succeeded" || got.ErrorCode != "" {
		t.Fatalf("stale snapshot overrode completed delete: %+v", got)
	}
}
