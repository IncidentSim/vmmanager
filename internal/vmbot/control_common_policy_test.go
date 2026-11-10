package vmbot

import (
	"context"
	"testing"
)

func TestProtectedImageNeverOverridesDurablePhase(t *testing.T) {
	for _, phase := range []string{"publishing", "deleting"} {
		t.Run(phase, func(t *testing.T) {
			r, id := commonRig(t)
			if e := r.st.Update(func(s *State) { s.Images[0].Phase = phase }); e != nil {
				t.Fatal(e)
			}
			r.c.control.SharedImages = append(r.c.control.SharedImages, id)
			for _, i := range r.c.ControlImages(22) {
				if i.ID == id && i.Phase == "ready" {
					t.Fatal("protected config bypassed durable phase")
				}
			}
			if _, e := r.c.AcceptControl(22, "boot-overlap", ControlRequest{Kind: "create", ImageID: id}); e == nil {
				t.Fatal("non-ready configured image admitted for boot")
			}
		})
	}
}
func TestPendingDeleteCannotRemoveNewlyProtectedImage(t *testing.T) {
	r, id := commonRig(t)
	op, e := r.c.AcceptControl(11, "delete-before-policy", ControlRequest{Kind: "delete_image", ImageID: id})
	if e != nil {
		t.Fatal(e)
	}
	r.c.control.SharedImages = append(r.c.control.SharedImages, id)
	if e = r.c.executeControl(context.Background(), op.ID); e != nil {
		t.Fatal(e)
	}
	got, e := r.c.ControlOperation(11, op.ID)
	if e != nil || got.Status != "failed" {
		t.Fatalf("pending delete not fenced: %+v %v", got, e)
	}
	if len(r.st.Snapshot().Images) != 1 {
		t.Fatal("protected image record removed")
	}
	imgs, e := r.c.lc.ListImages(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, i := range imgs {
		if i.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("protected physical image removed")
	}
}
