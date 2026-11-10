package vmbot

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
	"testing"
)

func TestControlStopCanRetryFailedAttempt(t *testing.T) {
	st, e := OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	c := NewController(svc, st, &controlTerms{}, ControllerConfig{Profile: "small"})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	o, _ := c.AcceptControl(11, "create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	c.executeControl(context.Background(), o.ID)
	stop, e := c.AcceptControl(11, "stop", ControlRequest{Kind: "stop", VMID: o.VMID})
	if e != nil {
		t.Fatal(e)
	}
	st.Update(func(s *State) {
		for n := range s.Operations {
			if s.Operations[n].ID == stop.ID {
				s.Operations[n].Status = "failed"
			}
		}
	})
	if _, e = c.AcceptControl(11, "retry-stop", ControlRequest{Kind: "stop", VMID: o.VMID}); e != nil {
		t.Fatal("failed stop not retryable", e)
	}
}

func TestControlDeleteSupersedesQueuedCommit(t *testing.T) {
	st, e := OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	c := NewController(svc, st, &controlTerms{}, ControllerConfig{Profile: "small"})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	create, e := c.AcceptControl(11, "create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if e != nil {
		t.Fatal(e)
	}
	if e = c.executeControl(context.Background(), create.ID); e != nil {
		t.Fatal(e)
	}
	commit, e := c.AcceptControl(11, "commit", ControlRequest{Kind: "commit", VMID: create.VMID, Name: "derived"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.AcceptControl(11, "delete", ControlRequest{Kind: "delete", VMID: create.VMID})
	if e != nil {
		t.Fatal(e)
	}
	if e = c.executeControl(context.Background(), commit.ID); e != nil {
		t.Fatal(e)
	}
	images, _ := svc.ListImages(context.Background())
	if len(images) != 1 {
		t.Fatal("superseded commit published image")
	}
}
