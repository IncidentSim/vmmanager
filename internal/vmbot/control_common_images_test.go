package vmbot

import (
	"context"
	"errors"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

func (r *imgRig) runAs(u int64, key string, req ControlRequest) ControlOperation {
	r.t.Helper()
	o, e := r.c.AcceptControl(u, key, req)
	if e != nil {
		r.t.Fatalf("accept %s: %v", key, e)
	}
	return awaitControl(r.t, r.c, u, o.ID)
}

// commonRig returns a rig where synthetic user 11 has committed image "derived".
// Users 11 and 22 are both admitted.
func commonRig(t *testing.T) (*imgRig, string) {
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	r := newImgRig(t, t.TempDir()+"/state", svc, 0)
	t.Cleanup(func() { r.close() })
	return r, r.derivedImage()
}

func (r *imgRig) sourceVM() string { return r.st.Snapshot().VMs[0].ID }

func TestCommonImageVisibleAndBootableByOtherUserButNotItsVM(t *testing.T) {
	t.Parallel()
	r, img := commonRig(t)
	src := r.sourceVM()
	got, ok := r.image(22, img)
	if !ok || got.Phase != "ready" || !got.Shared || got.CanDelete {
		t.Fatalf("user B view: %+v %v", got, ok)
	}
	if mine, _ := r.image(11, img); !mine.CanDelete || !mine.Shared {
		t.Fatalf("creator view: %+v", mine)
	}
	b := r.runAs(22, "boot-b", ControlRequest{Kind: "create", ImageID: img, Name: "b-vm"})
	if b.Status != "succeeded" {
		t.Fatalf("boot common image: %+v", b)
	}
	if vms := r.c.ControlVMs(22); len(vms) != 1 || vms[0].ID != b.VMID {
		t.Fatalf("B sees foreign VMs: %+v", vms)
	}
	for _, v := range r.c.ControlVMs(11) {
		if v.ID == b.VMID {
			t.Fatal("A sees B VM")
		}
	}
	if _, e := r.c.ControlVM(22, src); !errors.Is(e, ErrNoVM) {
		t.Fatalf("source VM leaked: %v", e)
	}
	if _, e := r.c.ControlOperation(22, "op-"+img[len("img-"):]); !errors.Is(e, ErrNoVM) {
		t.Fatalf("operation leaked: %v", e)
	}
	if _, e := r.c.ControlSSH(context.Background(), 22, src); !errors.Is(e, ErrNoVM) {
		t.Fatalf("ssh leaked: %v", e)
	}
	if _, e := r.c.ControlTerminalURL(22, src); !errors.Is(e, ErrNoVM) {
		t.Fatalf("terminal leaked: %v", e)
	}
}

func TestCommonImageDeleteCreatorOnlyAndGlobalReferenceGuard(t *testing.T) {
	t.Parallel()
	r, img := commonRig(t)
	if _, e := r.c.AcceptControl(22, "rm-b", ControlRequest{Kind: "delete_image", ImageID: img}); !errors.Is(e, ErrControlForbidden) {
		t.Fatalf("foreign delete: %v", e)
	}
	if got, _ := r.image(11, img); got.Phase != "ready" {
		t.Fatalf("foreign delete changed phase: %+v", got)
	}
	// A pending create by B (held at the backend) must fence A's delete.
	release := r.gate.arm("create")
	defer release()
	pend, e := r.c.AcceptControl(22, "boot-b", ControlRequest{Kind: "create", ImageID: img, Name: "b-vm"})
	if e != nil {
		t.Fatal(e)
	}
	r.gate.waitEntered(t, "create")
	if _, e = r.c.AcceptControl(11, "rm-a", ControlRequest{Kind: "delete_image", ImageID: img}); !errors.Is(e, ErrControlConflict) {
		t.Fatalf("pending create must fence delete: %v", e)
	}
	if got, _ := r.image(11, img); got.Phase != "ready" {
		t.Fatalf("refused delete changed phase: %+v", got)
	}
	release()
	if got := awaitControl(t, r.c, 22, pend.ID); got.Status != "succeeded" {
		t.Fatalf("B create %+v", got)
	}
	if _, e = r.c.AcceptControl(11, "rm-a2", ControlRequest{Kind: "delete_image", ImageID: img}); !errors.Is(e, ErrControlConflict) {
		t.Fatalf("running B VM must fence delete: %v", e)
	}
	r.runAs(22, "rm-vm", ControlRequest{Kind: "delete", VMID: pend.VMID})
	if d := r.runAs(11, "rm-a3", ControlRequest{Kind: "delete_image", ImageID: img}); d.Status != "succeeded" {
		t.Fatalf("delete after unreferenced: %+v", d)
	}
}

func TestCommonImageDeletingVisibleToCreatorOnlyAndNotBootable(t *testing.T) {
	t.Parallel()
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	r := newImgRig(t, t.TempDir()+"/state", svc, 1)
	defer r.close()
	img := r.derivedImage()
	if d := r.run("rm", ControlRequest{Kind: "delete_image", ImageID: img}); d.Status != "failed" {
		t.Fatal(d)
	}
	if _, ok := r.image(22, img); ok {
		t.Fatal("deleting image visible to other user")
	}
	if _, e := r.c.AcceptControl(22, "b", ControlRequest{Kind: "create", ImageID: img}); !errors.Is(e, ErrNoVM) {
		t.Fatalf("B boot deleting: %v", e)
	}
	if _, e := r.c.AcceptControl(11, "a", ControlRequest{Kind: "create", ImageID: img}); !errors.Is(e, ErrNoVM) {
		t.Fatalf("A boot deleting: %v", e)
	}
	if n := r.run("rm2", ControlRequest{Kind: "delete_image", ImageID: img}); n.Status != "succeeded" {
		t.Fatalf("creator retry: %+v", n)
	}
}

func TestSystemImageProtectedAndNotDeletable(t *testing.T) {
	t.Parallel()
	r, _ := commonRig(t)
	got, ok := r.image(11, "ubuntu")
	if !ok || !got.Shared || got.CanDelete {
		t.Fatalf("system image view: %+v %v", got, ok)
	}
	if _, e := r.c.AcceptControl(11, "rm-sys", ControlRequest{Kind: "delete_image", ImageID: "ubuntu"}); !errors.Is(e, ErrControlConflict) {
		t.Fatalf("system delete: %v", e)
	}
}

func TestCommonImageNamesReservedGloballyForNewCommits(t *testing.T) {
	t.Parallel()
	r, _ := commonRig(t)
	src := r.sourceVM()
	b := r.runAs(22, "b-create", ControlRequest{Kind: "create", ImageID: "ubuntu", Name: "b-src"})
	if b.Status != "succeeded" {
		t.Fatal(b)
	}
	r.runAs(22, "b-stop", ControlRequest{Kind: "stop", VMID: b.VMID})
	for _, name := range []string{"derived", "ubuntu"} {
		if _, e := r.c.AcceptControl(22, "c-"+name, ControlRequest{Kind: "commit", VMID: b.VMID, Name: name}); !errors.Is(e, ErrControlConflict) {
			t.Fatalf("name %q: %v", name, e)
		}
	}
	// Replay of the original commit key stays idempotent despite the reservation.
	again, e := r.c.AcceptControl(11, "commit", ControlRequest{Kind: "commit", VMID: src, Name: "derived"})
	if e != nil || again.Status != "succeeded" {
		t.Fatalf("replay: %+v %v", again, e)
	}
	if c := r.runAs(22, "c-ok", ControlRequest{Kind: "commit", VMID: b.VMID, Name: "other"}); c.Status != "succeeded" {
		t.Fatal(c)
	}
}

func TestCommonImageNameRaceHasOneWinner(t *testing.T) {
	t.Parallel()
	r, _ := commonRig(t)
	a := r.runAs(22, "b-create", ControlRequest{Kind: "create", ImageID: "ubuntu", Name: "b-src"})
	r.runAs(22, "b-stop", ControlRequest{Kind: "stop", VMID: a.VMID})
	a2 := r.runAs(11, "a-create2", ControlRequest{Kind: "create", ImageID: "ubuntu", Name: "a-src2"})
	if a2.Status != "succeeded" {
		t.Fatal(a2)
	}
	r.runAs(11, "a-stop2", ControlRequest{Kind: "stop", VMID: a2.VMID})
	ch := make(chan error, 2)
	go func() {
		_, e := r.c.AcceptControl(22, "race-b", ControlRequest{Kind: "commit", VMID: a.VMID, Name: "race"})
		ch <- e
	}()
	go func() {
		_, e := r.c.AcceptControl(11, "race-a", ControlRequest{Kind: "commit", VMID: a2.VMID, Name: "race"})
		ch <- e
	}()
	x, y := <-ch, <-ch
	if (x == nil) == (y == nil) {
		t.Fatalf("exactly one winner expected: %v %v", x, y)
	}
	n := 0
	for _, i := range r.st.Snapshot().Images {
		if i.Name == "race" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("race records: %d", n)
	}
}

func TestLegacyPrivateImageBecomesCommonPreservingIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() + "/state"
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	r := newImgRig(t, dir, svc, 0)
	img := r.derivedImage()
	before := r.st.Snapshot().Images
	r.close()
	r2 := newImgRig(t, dir, svc, 0)
	defer r2.close()
	after := r2.st.Snapshot().Images
	if len(after) != 1 || after[0] != before[0] || after[0].Shared {
		t.Fatalf("stored record changed: %+v -> %+v", before, after)
	}
	got, ok := r2.image(22, img)
	if !ok || got.Name != "derived" || got.OwnerUserID != 11 || got.Phase != "ready" || !got.Shared || got.CanDelete {
		t.Fatalf("legacy view: %+v %v", got, ok)
	}
}

func TestLegacyDuplicateNamesRemainListedWithStableIDs(t *testing.T) {
	t.Parallel()
	r, _ := commonRig(t)
	if e := r.st.Update(func(s *State) {
		s.Images = append(s.Images, ControlImage{ID: "img-legacydup", Name: "derived", OwnerUserID: 22, Phase: "ready"})
	}); e != nil {
		t.Fatal(e)
	}
	n := 0
	for _, i := range r.c.ControlImages(22) {
		if i.Name == "derived" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("both duplicates must remain listed, got %d", n)
	}
}
