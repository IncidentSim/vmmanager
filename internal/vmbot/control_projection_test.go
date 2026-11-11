package vmbot

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
	"testing"
	"time"
)

func TestControlRecoveryRestoresOnlyAllowedActiveProjection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() + "/state"
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	policy := func(users ...int64) ControlPolicy {
		return ControlPolicy{AllowedUsers: users, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1}
	}
	open := func(terms Terminals, users ...int64) (*Store, *Controller) {
		st, e := OpenControlStore(dir, 0)
		if e != nil {
			t.Fatal(e)
		}
		c := NewController(svc, st, terms, ControllerConfig{Profile: "small"})
		c.ConfigureControl(policy(users...))
		if e = c.Start(context.Background()); e != nil {
			t.Fatal(e)
		}
		if e = c.RecoverControl(); e != nil {
			t.Fatal(e)
		}
		return st, c
	}
	st, c := open(&fakeTerms{}, 11, 22)
	for _, u := range []int64{11, 22} {
		o, e := c.AcceptControl(u, "create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
		if e != nil {
			t.Fatal(e)
		}
		awaitControl(t, c, u, o.ID)
	}
	_ = c.Close()
	st.Close()
	// Restart with user 22 no longer allowed: only user 11's running VM gets its
	// terminal rebuilt, and nothing is deleted.
	terms := &fakeTerms{}
	st, c = open(terms, 11)
	defer func() { _ = c.Close(); st.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		terms.mu.Lock()
		n := len(terms.started)
		terms.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	terms.mu.Lock()
	defer terms.mu.Unlock()
	if len(terms.started) != 1 {
		t.Fatalf("restored %d terminals want one", len(terms.started))
	}
	if len(st.Snapshot().VMs) != 2 {
		t.Fatal("restart changed VM records")
	}
}
