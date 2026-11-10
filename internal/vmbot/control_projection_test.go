package vmbot

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
	"testing"
)

func TestControlRecoveryRestoresOnlyAllowedActiveProjection(t *testing.T) {
	st, e := OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	terms := &fakeTerms{}
	c := NewController(svc, st, terms, ControllerConfig{Profile: "small"})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11, 22}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	for _, u := range []int64{11, 22} {
		o, e := c.AcceptControl(u, "create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
		if e != nil {
			t.Fatal(e)
		}
		if e = c.executeControl(context.Background(), o.ID); e != nil {
			t.Fatal(e)
		}
	}
	c.control.AllowedUsers = []int64{11}
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.RecoverControl(); e != nil {
		t.Fatal(e)
	}
	// Wait for queued recovery, not just completed old creates.
	for _, o := range st.Snapshot().Operations {
		if o.OwnerUserID == 11 {
			awaitControl(t, c, 11, o.ID)
		}
	}
	terms.mu.Lock()
	defer terms.mu.Unlock()
	if len(terms.started) != 1 {
		t.Fatalf("restored %d terminals want one", len(terms.started))
	}
}
