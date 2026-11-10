package vmbot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

type commitHook struct {
	vmlib.Lifecycle
	commit func() error
}

func (h *commitHook) Commit(ctx context.Context, id, img string) (vmlib.Image, error) {
	if e := h.commit(); e != nil {
		return vmlib.Image{}, e
	}
	return h.Lifecycle.Commit(ctx, id, img)
}

// Lifecycle.Commit exposes no durable source/publication provenance across a
// backend restart, so an AlreadyExists (preexisting collision OR our own crashed
// publication) or an ambiguous transport error can never be adopted. The image
// stays private/unpublished, unbootable and undeletable through the API, the
// operation fails safely, and the source VM can still be deleted explicitly.
func TestControlCommitAmbiguityFailsClosedAndNeverAdopts(t *testing.T) {
	for name, commitErr := range map[string]error{"collision": vmlib.ErrAlreadyExists, "ambiguous": errors.New("lost response")} {
		t.Run(name, func(t *testing.T) {
			st, e := OpenControlStore(t.TempDir()+"/state", 0)
			if e != nil {
				t.Fatal(e)
			}
			defer st.Close()
			svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
			c := NewController(&commitHook{Lifecycle: svc, commit: func() error { return commitErr }}, st, &controlTerms{}, ControllerConfig{Profile: "small", OpTimeout: 2 * time.Second})
			c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 2})
			if e = c.Start(context.Background()); e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			wait := func(id, want string) ControlOperation {
				t.Helper()
				for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
					if o, _ := c.ControlOperation(11, id); o.Status == want {
						return o
					}
				}
				t.Fatalf("operation %s never %s", id, want)
				return ControlOperation{}
			}
			a, e := c.AcceptControl(11, "c", ControlRequest{Kind: "create", ImageID: "ubuntu"})
			if e != nil {
				t.Fatal(e)
			}
			wait(a.ID, "succeeded")
			cm, e := c.AcceptControl(11, "m", ControlRequest{Kind: "commit", VMID: a.VMID, Name: "derived"})
			if e != nil {
				t.Fatal(e)
			}
			wait(cm.ID, "failed")
			for _, i := range c.ControlImages(11) {
				if i.ID == cm.ImageID {
					t.Fatal("unproven publication became visible/ready")
				}
			}
			if _, e = c.AcceptControl(11, "boot", ControlRequest{Kind: "create", ImageID: cm.ImageID, Name: "x"}); !errors.Is(e, ErrNoVM) {
				t.Fatalf("boot from unproven image: %v", e)
			}
			if _, e = c.AcceptControl(11, "rm", ControlRequest{Kind: "delete_image", ImageID: cm.ImageID}); !errors.Is(e, ErrNoVM) {
				t.Fatalf("API deletion of unproven image: %v", e)
			}
			// Name stays reserved: no silent re-publication under a new random ID.
			if _, e = c.AcceptControl(11, "again", ControlRequest{Kind: "commit", VMID: a.VMID, Name: "derived"}); e == nil {
				t.Fatal("blind retry accepted")
			}
			found := false
			for _, i := range st.Snapshot().Images {
				found = found || (i.ID == cm.ImageID && i.Phase == "publishing" && i.OwnerUserID == 11)
			}
			if !found {
				t.Fatal("durable private publication marker lost")
			}
			d, e := c.AcceptControl(11, "del", ControlRequest{Kind: "delete", VMID: a.VMID})
			if e != nil {
				t.Fatal(e)
			}
			wait(d.ID, "succeeded")
		})
	}
}
