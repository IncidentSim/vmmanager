package vmbot

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"sync"
	"testing"
	"time"
)

type blockingRevokeTerms struct {
	Terminals
	id               string
	entered, release chan struct{}
	once             sync.Once
}

func (b *blockingRevokeTerms) Revoke(id string) {
	if id == b.id {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
	b.Terminals.Revoke(id)
}

func TestOrchestrationSlowTerminalCleanupDoesNotBlockOtherVMDelete(t *testing.T) {
	e := newEnvMax(t, 2)
	a := mustCreate(t, e, "a")
	b := mustCreate(t, e, "b")
	blocker := &blockingRevokeTerms{Terminals: e.terms, id: a.VMID, entered: make(chan struct{}), release: make(chan struct{})}
	e.ctl.term = blocker
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocker.release) }) }
	t.Cleanup(release)
	adone := make(chan error, 1)
	go func() { _, err := e.ctl.Delete(context.Background(), Ref{Sel: "a"}); adone <- err }()
	select {
	case <-blocker.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no revoke")
	}
	bdone := make(chan error, 1)
	go func() { _, err := e.ctl.Delete(context.Background(), Ref{Sel: "b"}); bdone <- err }()
	select {
	case err := <-bdone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		release()
		<-adone
		<-bdone
		t.Fatal("unrelated delete blocked behind another VM terminal cleanup")
	}
	if _, err := e.svc.Get(context.Background(), b.VMID); !errors.Is(err, vmlib.ErrNotFound) {
		t.Fatalf("b not deleted: %v", err)
	}
	release()
	if err := <-adone; err != nil {
		t.Fatal(err)
	}
}

func TestOrchestrationWorkflowIDCollisionCannotRetarget(t *testing.T) {
	e := newEnvMax(t, 2)
	a := mustCreate(t, e, "a")
	b := mustCreate(t, e, "b")
	if _, err := e.ctl.submit(OpInput{Kind: opDelete, VMID: b.VMID, Seq: 1}, createID(a.VMID)); err == nil {
		t.Fatal("workflow ID reused with another intent")
	}
	if e.be.Count() != 2 {
		t.Fatal("ID conflict must not touch either VM")
	}
}

func TestOrchestrationDuplicateDeleteAttachesToSameAttempt(t *testing.T) {
	e := newEnv(t)
	a := mustCreate(t, e, "a")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	calls := 0
	e.lc.del = func(ctx context.Context, id string) (vmlib.DeleteResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		once.Do(func() { close(entered) })
		<-release
		return e.svc.Delete(ctx, id)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	first := make(chan error, 1)
	go func() { _, err := e.ctl.Delete(context.Background(), Ref{Sel: "a"}); first <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("delete not entered")
	}
	// Submit while the first effect is still running: deterministic concurrent
	// duplicate command, without racing the completion/removal of the record.
	h, rec, err := e.ctl.submitDelete(e.only(t))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Seq != 1 || h.GetWorkflowID() != deleteID(a.VMID, 1) {
		t.Fatal("duplicate advanced the intent sequence")
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if r, err := e.ctl.wait(h, time.Second); err != nil || r.err() != nil {
		t.Fatalf("duplicate outcome: %v %+v", err, r)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("duplicate effect calls=%d", calls)
	}
}
