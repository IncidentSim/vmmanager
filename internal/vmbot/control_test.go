package vmbot

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
	"testing"
	"time"
)

func TestControlDurableAdmission(t *testing.T) {
	st, e := OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	svc, e := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	if e != nil {
		t.Fatal(e)
	}
	c := NewController(svc, st, &controlTerms{}, ControllerConfig{Profile: "small"})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11, 22}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	req := ControlRequest{Kind: "create", Name: "lab", ImageID: "ubuntu"}
	a, e := c.AcceptControl(11, "same", req)
	if e != nil {
		t.Fatal(e)
	}
	b, e := c.AcceptControl(11, "same", req)
	if e != nil || a.ID != b.ID || a.VMID != b.VMID {
		t.Fatalf("duplicate %v %v", b, e)
	}
	req.Name = "other"
	if _, e = c.AcceptControl(11, "same", req); !errors.Is(e, ErrControlConflict) {
		t.Fatal(e)
	}
	if _, e = c.AcceptControl(11, "next", req); !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
	if _, e = c.ControlVM(22, a.VMID); !errors.Is(e, ErrNoVM) {
		t.Fatal(e)
	}
	if _, e = c.AcceptControl(22, "foreign", ControlRequest{Kind: "delete", VMID: a.VMID}); !errors.Is(e, ErrNoVM) {
		t.Fatal(e)
	}
	if len(st.Snapshot().Operations) != 1 {
		t.Fatal("foreign mutation persisted")
	}
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	if e = c.RecoverControl(); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		o, _ := c.ControlOperation(11, a.ID)
		if o.Status == "succeeded" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("operation did not complete")
}

type controlTerms struct{}

func (*controlTerms) Start(context.Context, TerminalTarget) error { return nil }
func (*controlTerms) Revoke(string)                               {}
func (*controlTerms) Close()                                      {}

var _ = vmlib.ErrNotFound

// A backend policy refusal after durable admission must surface as a stable,
// safe operation error code, not a blanket "unavailable".
func TestControlOperationErrorCodeForBackendRefusal(t *testing.T) {
	st, e := OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	c := NewController(svc, st, &controlTerms{}, ControllerConfig{Profile: "small", OpTimeout: 2 * time.Second})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	a, e := c.AcceptControl(11, "huge", ControlRequest{Kind: "create", ImageID: "ubuntu", Resources: &ControlResources{VCPU: 1, MemoryMiB: 1 << 30}})
	if e != nil {
		t.Fatal(e)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		o, _ := c.ControlOperation(11, a.ID)
		if o.Status == "failed" {
			t.Logf("code=%s", o.ErrorCode)
			if o.ErrorCode != "invalid_argument" && o.ErrorCode != "capacity" {
				t.Fatalf("code %q", o.ErrorCode)
			}
			// Documented behavior: the failed record keeps its slot until an
			// explicit Delete, and that Delete can complete.
			if v, e := c.ControlVM(11, a.VMID); e != nil || v.Phase != PhaseCreating {
				t.Fatalf("failed create record %v %+v", e, v)
			}
			d, e := c.AcceptControl(11, "cleanup", ControlRequest{Kind: "delete", VMID: a.VMID})
			if e != nil {
				t.Fatal(e)
			}
			for end2 := time.Now().Add(5 * time.Second); time.Now().Before(end2); time.Sleep(10 * time.Millisecond) {
				if x, _ := c.ControlOperation(11, d.ID); x.Status == "succeeded" {
					if _, e = c.ControlVM(11, a.VMID); !errors.Is(e, ErrNoVM) {
						t.Fatal("deleted record still visible")
					}
					return
				}
			}
			t.Fatal("cleanup delete did not complete")
			return
		}
		if o.Status == "succeeded" {
			t.Fatal("out-of-policy resources succeeded")
		}
	}
	t.Fatal("not failed")
}
