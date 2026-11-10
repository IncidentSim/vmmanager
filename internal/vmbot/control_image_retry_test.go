package vmbot

import (
	"context"
	"errors"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// failFirstDeleteImage fails the first DeleteImage call before any effect.
type failFirstDeleteImage struct {
	vmlib.Lifecycle
	calls int
	fail  int // number of leading calls that fail
}

func (l *failFirstDeleteImage) DeleteImage(ctx context.Context, id string) error {
	l.calls++
	if l.calls <= l.fail {
		return errors.New("injected transient transport failure before effect")
	}
	return l.Lifecycle.DeleteImage(ctx, id)
}

type imgRig struct {
	t   *testing.T
	st  *Store
	c   *Controller
	svc *vmlib.Service
	lc  *failFirstDeleteImage
}

// newImgRig opens (or reopens) dir and builds a controller whose engine is not
// started: operations are executed synchronously with executeControl.
func newImgRig(t *testing.T, dir string, svc *vmlib.Service, fail int) *imgRig {
	t.Helper()
	st, e := OpenControlStore(dir, 0)
	if e != nil {
		t.Fatal(e)
	}
	lc := &failFirstDeleteImage{Lifecycle: svc, fail: fail}
	c := NewController(lc, st, &controlTerms{}, ControllerConfig{Profile: "small"})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11, 22}, SharedImages: []string{"ubuntu"}, MaxVMs: 4, MaxVMsPerUser: 4})
	return &imgRig{t, st, c, svc, lc}
}

func (r *imgRig) run(key string, req ControlRequest) ControlOperation {
	r.t.Helper()
	o, e := r.c.AcceptControl(11, key, req)
	if e != nil {
		r.t.Fatalf("accept %s: %v", key, e)
	}
	if e = r.c.executeControl(context.Background(), o.ID); e != nil {
		r.t.Fatal(e)
	}
	o, _ = r.c.ControlOperation(11, o.ID)
	return o
}

// derivedImage creates a VM and commits a private image named "derived".
func (r *imgRig) derivedImage() string {
	r.t.Helper()
	cr := r.run("create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if cr.Status != "succeeded" {
		r.t.Fatal(cr)
	}
	cm := r.run("commit", ControlRequest{Kind: "commit", VMID: cr.VMID, Name: "derived"})
	if cm.Status != "succeeded" {
		r.t.Fatal(cm)
	}
	return cm.ImageID
}

func (r *imgRig) image(u int64, id string) (ControlImage, bool) {
	for _, i := range r.c.ControlImages(u) {
		if i.ID == id {
			return i, true
		}
	}
	return ControlImage{}, false
}

func (r *imgRig) durable(id string) bool {
	for _, i := range r.st.Snapshot().Images {
		if i.ID == id {
			return true
		}
	}
	return false
}

func TestControlDeleteImageTransientFailureRetriesWithNewKey(t *testing.T) {
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	r := newImgRig(t, t.TempDir()+"/state", svc, 1)
	defer r.st.Close()
	img := r.derivedImage()

	d := r.run("rm", ControlRequest{Kind: "delete_image", ImageID: img})
	if d.Status != "failed" {
		t.Fatalf("first delete should fail: %+v", d)
	}
	// Owner still sees the image, with its deleting phase; it is not bootable.
	got, ok := r.image(11, img)
	if !ok || got.Phase != "deleting" || got.Shared {
		t.Fatalf("owner visibility lost: %+v %v", got, ok)
	}
	if _, ok := r.image(22, img); ok {
		t.Fatal("deleting image visible to another owner")
	}
	if _, e := r.c.AcceptControl(11, "boot", ControlRequest{Kind: "create", ImageID: img, Name: "x"}); !errors.Is(e, ErrNoVM) {
		t.Fatalf("deleting image must not boot: %v", e)
	}
	// Same key: original failed operation, no new backend attempt.
	same, e := r.c.AcceptControl(11, "rm", ControlRequest{Kind: "delete_image", ImageID: img})
	if e != nil || same.ID != d.ID || same.Status != "failed" || r.lc.calls != 1 {
		t.Fatalf("same key replay: %+v %v calls=%d", same, e, r.lc.calls)
	}
	// Another owner cannot retry it.
	if _, e = r.c.AcceptControl(22, "x", ControlRequest{Kind: "delete_image", ImageID: img}); !errors.Is(e, ErrNoVM) {
		t.Fatalf("foreign retry: %v", e)
	}
	// New key after a terminal failure is a fresh attempt and succeeds.
	n := r.run("rm-2", ControlRequest{Kind: "delete_image", ImageID: img})
	if n.ID == d.ID || n.Status != "succeeded" || r.lc.calls != 2 {
		t.Fatalf("retry: %+v calls=%d", n, r.lc.calls)
	}
	if r.durable(img) {
		t.Fatal("image record survived successful delete")
	}
	// Original key still maps to the original failed operation.
	again, _ := r.c.AcceptControl(11, "rm", ControlRequest{Kind: "delete_image", ImageID: img})
	if again.ID != d.ID || again.Status != "failed" {
		t.Fatalf("original result rewritten: %+v", again)
	}
	// Name reservation is released.
	if o := r.run("recommit", ControlRequest{Kind: "create", ImageID: "ubuntu", Name: "again"}); o.Status != "succeeded" {
		t.Fatal(o)
	}
}

func TestControlDeleteImageRetryAfterRestart(t *testing.T) {
	dir := t.TempDir() + "/state"
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	r := newImgRig(t, dir, svc, 1)
	img := r.derivedImage()
	if d := r.run("rm", ControlRequest{Kind: "delete_image", ImageID: img}); d.Status != "failed" {
		t.Fatal(d)
	}
	r.st.Close()

	r2 := newImgRig(t, dir, svc, 0)
	defer r2.st.Close()
	if got, ok := r2.image(11, img); !ok || got.Phase != "deleting" {
		t.Fatalf("after restart: %+v %v", got, ok)
	}
	if n := r2.run("rm-2", ControlRequest{Kind: "delete_image", ImageID: img}); n.Status != "succeeded" {
		t.Fatalf("retry after restart: %+v", n)
	}
	if r2.durable(img) {
		t.Fatal("image survived")
	}
}

func TestControlDeleteImageRetryWhenRemoteAlreadyAbsent(t *testing.T) {
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	r := newImgRig(t, t.TempDir()+"/state", svc, 1)
	defer r.st.Close()
	img := r.derivedImage()
	if d := r.run("rm", ControlRequest{Kind: "delete_image", ImageID: img}); d.Status != "failed" {
		t.Fatal(d)
	}
	// The first attempt actually landed remotely (lost response).
	if e := svc.DeleteImage(context.Background(), img); e != nil {
		t.Fatal(e)
	}
	if n := r.run("rm-2", ControlRequest{Kind: "delete_image", ImageID: img}); n.Status != "succeeded" {
		t.Fatalf("absent-remote retry: %+v", n)
	}
	if r.durable(img) {
		t.Fatal("image survived")
	}
}

func TestControlDeleteImageRetryRefusedWhileOperationActiveOrUnsafe(t *testing.T) {
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	r := newImgRig(t, t.TempDir()+"/state", svc, 0)
	defer r.st.Close()
	img := r.derivedImage()

	// Accepted but not executed: the previous operation is not terminal.
	first, e := r.c.AcceptControl(11, "rm", ControlRequest{Kind: "delete_image", ImageID: img})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.c.AcceptControl(11, "rm-2", ControlRequest{Kind: "delete_image", ImageID: img}); !errors.Is(e, ErrControlConflict) {
		t.Fatalf("concurrent fresh attempt must conflict: %v", e)
	}
	if e = r.c.executeControl(context.Background(), first.ID); e != nil {
		t.Fatal(e)
	}
	if o, _ := r.c.ControlOperation(11, first.ID); o.Status != "succeeded" {
		t.Fatal(o)
	}
	// Shared and unknown images are never deletable.
	if _, e = r.c.AcceptControl(11, "shared", ControlRequest{Kind: "delete_image", ImageID: "ubuntu"}); !errors.Is(e, ErrControlConflict) {
		t.Fatalf("shared: %v", e)
	}
	if _, e = r.c.AcceptControl(11, "unknown", ControlRequest{Kind: "delete_image", ImageID: "img-unknown"}); !errors.Is(e, ErrNoVM) {
		t.Fatalf("unknown: %v", e)
	}
	// A deleting image referenced by a VM is never deleted.
	img2 := r.derivedImageNamed("second")
	r.forceDeleting(img2)
	r.st.Update(func(s *State) { s.VMs[0].ImageID = img2 })
	if _, e = r.c.AcceptControl(11, "ref", ControlRequest{Kind: "delete_image", ImageID: img2}); !errors.Is(e, ErrControlConflict) {
		t.Fatalf("referenced: %v", e)
	}
}

func (r *imgRig) derivedImageNamed(name string) string {
	r.t.Helper()
	cr := r.run("create-"+name, ControlRequest{Kind: "create", ImageID: "ubuntu", Name: name})
	if cr.Status != "succeeded" {
		r.t.Fatal(cr)
	}
	cm := r.run("commit-"+name, ControlRequest{Kind: "commit", VMID: cr.VMID, Name: "img-" + name})
	if cm.Status != "succeeded" {
		r.t.Fatal(cm)
	}
	return cm.ImageID
}

func (r *imgRig) forceDeleting(id string) {
	r.t.Helper()
	r.st.Update(func(s *State) {
		for n := range s.Images {
			if s.Images[n].ID == id {
				s.Images[n].Phase = "deleting"
			}
		}
	})
}
