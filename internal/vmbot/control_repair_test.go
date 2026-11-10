package vmbot

import (
	"context"
	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
	"testing"
	"time"
)

func TestControlTerminalWorkflowErrorRepair(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "after"}[exists], func(t *testing.T) {
			st, e := OpenControlStore(t.TempDir()+"/state", 0)
			if e != nil {
				t.Fatal(e)
			}
			defer st.Close()
			svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
			lc := &hookLC{Lifecycle: svc}
			creates := 0
			lc.create = func(ctx context.Context, r vmlib.CreateRequest) (vmlib.VM, error) {
				creates++
				if exists {
					svc.Create(ctx, r)
				}
				panic("injected durable workflow failure")
			}
			c := NewController(lc, st, &controlTerms{}, ControllerConfig{Profile: "small"})
			c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
			if e = c.Start(context.Background()); e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			o, e := c.AcceptControl(11, "create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
			if e != nil {
				t.Fatal(e)
			}
			end := time.Now().Add(3 * time.Second)
			for time.Now().Before(end) {
				_, status := c.lookup("control-" + o.ID)
				if status != nil && status.Status == dbos.WorkflowStatusError {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			lc.create = func(context.Context, vmlib.CreateRequest) (vmlib.VM, error) {
				creates++
				return vmlib.VM{}, vmlib.ErrAlreadyExists
			}
			if e = c.RecoverControl(); e != nil {
				t.Fatal(e)
			}
			got := awaitControl(t, c, 11, o.ID)
			if got.Status == "running" || got.Status == "pending" {
				t.Fatal("stranded workflow")
			}
			if creates != 1 {
				t.Fatal("repair recreated VM")
			}
			if exists && got.Status != "succeeded" {
				t.Fatalf("not adopted %+v", got)
			}
			if !exists && len(st.Snapshot().VMs) != 0 {
				t.Fatal("absence barrier failed")
			}
		})
	}
}
