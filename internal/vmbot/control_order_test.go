package vmbot

import (
	"context"
	"errors"
	"testing"
)

func TestControlStopCanRetryFailedAttempt(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("create")
	e.lc.mu.Lock()
	e.lc.fail["stop"] = errors.New("injected backend failure")
	e.lc.mu.Unlock()
	stop := e.accept("stop", ControlRequest{Kind: "stop", VMID: c.VMID})
	if got := awaitControl(t, e.ctl, ownerUser, stop.ID); got.Status != "failed" {
		t.Fatalf("injected failure not recorded: %+v", got)
	}
	e.lc.mu.Lock()
	delete(e.lc.fail, "stop")
	e.lc.mu.Unlock()
	retry, err := e.ctl.AcceptControl(ownerUser, "retry-stop", ControlRequest{Kind: "stop", VMID: c.VMID})
	if err != nil {
		t.Fatal("failed stop not retryable", err)
	}
	if got := awaitControl(t, e.ctl, ownerUser, retry.ID); got.Status != "succeeded" {
		t.Fatalf("retry %+v", got)
	}
}

// Agreed sequential semantics (docs/rfc-vm-owner.md): a Delete delivered behind a
// queued Commit no longer supersedes it. The earlier command runs to its
// outcome first; only then is the VM deleted. (Before the owner model the
// Delete won and the Commit never published.)
func TestControlDeleteWaitsForQueuedCommit(t *testing.T) {
	t.Parallel()
	e := newOwnerEnv(t)
	c := e.create("create")
	release := e.lc.arm("commit")
	defer release()
	commit := e.accept("commit", ControlRequest{Kind: "commit", VMID: c.VMID, Name: "derived"})
	e.lc.waitEntered(t, "commit")
	del := e.accept("delete", ControlRequest{Kind: "delete", VMID: c.VMID})
	release()
	if got := awaitControl(t, e.ctl, ownerUser, commit.ID); got.Status != "succeeded" {
		t.Fatalf("commit %+v", got)
	}
	if got := awaitControl(t, e.ctl, ownerUser, del.ID); got.Status != "succeeded" {
		t.Fatalf("delete %+v", got)
	}
	images, _ := e.svc.ListImages(context.Background())
	if len(images) != 2 { // base image + the committed one
		t.Fatalf("earlier commit must publish before delete: %d images", len(images))
	}
}
