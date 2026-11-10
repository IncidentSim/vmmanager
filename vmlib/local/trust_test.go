//go:build linux

package local_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

func mustNotStart(t *testing.T, r *rig, why string) {
	t.Helper()
	if b, err := local.New(r.cfg); err == nil {
		b.Close()
		t.Fatalf("%s: New accepted an unsafe layout", why)
	}
	if _, err := os.Stat(filepath.Join(r.root, "vms")); !os.IsNotExist(err) {
		t.Fatalf("%s: VM workspace written before the layout was validated", why)
	}
}

// ---- Finding 6: root-owned runtime tree and run lock ----------------------------

func TestWritableRuntimeParentIsRejectedBeforeAnythingIsWritten(t *testing.T) {
	for _, mode := range []os.FileMode{0o770, 0o775, 0o777, 0o722} {
		r := newRig(t, 1)
		if err := os.Chmod(r.run, mode); err != nil {
			t.Fatal(err)
		}
		mustNotStart(t, r, "runtime dir mode "+mode.String())
	}
}

func TestWritableAncestorOfRuntimeDirIsRejected(t *testing.T) {
	r := newRig(t, 1)
	parent := filepath.Dir(r.run)
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0o755)
	mustNotStart(t, r, "group-writable ancestor")
}

func TestRuntimeDirOwnedByAnotherUserIsRejected(t *testing.T) {
	r := newRig(t, 1)
	other := uint32(os.Getuid()) + 1
	r.cfg.TrustedOwnerUID = &other // the real owner is no longer the trusted one
	mustNotStart(t, r, "untrusted owner")
}

func TestMissingOrUnsafeRunLockIsRejected(t *testing.T) {
	lock := func(r *rig) string { return filepath.Join(r.run, ".backend.lock") }
	cases := map[string]func(r *rig){
		"missing (the operator must precreate it)": func(r *rig) { os.Remove(lock(r)) },
		"symlink": func(r *rig) {
			os.Remove(lock(r))
			os.WriteFile(filepath.Join(filepath.Dir(r.run), "elsewhere.lock"), nil, 0o600)
			os.Symlink(filepath.Join(filepath.Dir(r.run), "elsewhere.lock"), lock(r))
		},
		"world-writable": func(r *rig) { os.Chmod(lock(r), 0o666) },
		"directory": func(r *rig) {
			os.Remove(lock(r))
			os.Mkdir(lock(r), 0o700)
		},
	}
	for name, mutate := range cases {
		r := newRig(t, 1)
		mutate(r)
		mustNotStart(t, r, name)
	}
}

func TestPrecreatedGroupWritableLockIsTheLegitimateLayout(t *testing.T) {
	r := newRig(t, 1)
	if err := os.Chmod(filepath.Join(r.run, ".backend.lock"), 0o660); err != nil {
		t.Fatal(err)
	}
	b, err := local.New(r.cfg)
	if err != nil {
		t.Fatalf("root-owned group-writable precreated lock must work: %v", err)
	}
	defer b.Close()
	// ...and it still excludes a second owner.
	if b2, err := local.New(r.cfg); err == nil {
		b2.Close()
		t.Fatal("run lock did not exclude a second owner")
	}
}

func TestUnsafeExistingSlotDirectoryBlocksStartup(t *testing.T) {
	r := newRig(t, 1)
	evil := filepath.Join(filepath.Dir(r.run), "evil")
	os.Mkdir(evil, 0o755)
	if err := os.Symlink(evil, filepath.Join(r.run, "slot-0")); err != nil {
		t.Fatal(err)
	}
	mustNotStart(t, r, "symlinked slot dir")
	os.Remove(filepath.Join(r.run, "slot-0"))
	os.Mkdir(filepath.Join(r.run, "slot-0"), 0o777)
	os.Chmod(filepath.Join(r.run, "slot-0"), 0o777)
	mustNotStart(t, r, "world-writable slot dir")
}

// A client-forged receipt must never count as proof of exit.
func TestForgedReceiptsAreNeverAccepted(t *testing.T) {
	forge := map[string]func(r *rig){
		"symlinked slot directory": func(r *rig) {
			evil := filepath.Join(filepath.Dir(r.run), "evil")
			os.MkdirAll(evil, 0o755)
			os.WriteFile(filepath.Join(evil, inv+".exit.json"), []byte(good()), 0o640)
			os.Symlink(evil, filepath.Join(r.run, "slot-0"))
		},
		"symlinked receipt": func(r *rig) {
			os.MkdirAll(filepath.Join(r.run, "slot-0"), 0o755)
			real := filepath.Join(filepath.Dir(r.run), "real.json")
			os.WriteFile(real, []byte(good()), 0o640)
			os.Symlink(real, filepath.Join(r.run, "slot-0", inv+".exit.json"))
		},
		"group-writable receipt": func(r *rig) {
			os.MkdirAll(filepath.Join(r.run, "slot-0"), 0o755)
			p := filepath.Join(r.run, "slot-0", inv+".exit.json")
			os.WriteFile(p, []byte(good()), 0o660)
			os.Chmod(p, 0o660)
		},
		"world-writable receipt": func(r *rig) {
			os.MkdirAll(filepath.Join(r.run, "slot-0"), 0o755)
			p := filepath.Join(r.run, "slot-0", inv+".exit.json")
			os.WriteFile(p, []byte(good()), 0o666)
			os.Chmod(p, 0o666)
		},
		"group-writable slot directory": func(r *rig) {
			d := filepath.Join(r.run, "slot-0")
			os.MkdirAll(d, 0o755)
			os.WriteFile(filepath.Join(d, inv+".exit.json"), []byte(good()), 0o640)
			os.Chmod(d, 0o775)
		},
		"receipt is a directory": func(r *rig) {
			os.MkdirAll(filepath.Join(r.run, "slot-0", inv+".exit.json"), 0o755)
		},
	}
	for name, f := range forge {
		r := newRig(t, 1)
		b := r.backend()
		startRunning(t, r, b, "a") // slot dir does not exist yet, so the layout was valid at start
		f(r)
		c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		if _, err := b.WaitExit(c, "a"); err == nil {
			t.Errorf("%s: WaitExit accepted a forged receipt", name)
		}
		cancel()
		if res, _ := b.Release(ctx, "a"); res.Released {
			t.Errorf("%s: Release freed a disk on a forged receipt", name)
		}
		if _, err := b.Publish(ctx, "a", "stolen"); err == nil {
			t.Errorf("%s: Publish accepted a forged receipt", name)
		}
		if _, err := os.Stat(filepath.Join(r.images, "stolen.ext4")); err == nil {
			t.Errorf("%s: image published from a forged receipt", name)
		}
		if h, _ := b.Inspect(ctx, "a"); h.State == vmlib.HostExited {
			t.Errorf("%s: VM marked exited by a forged receipt", name)
		}
	}
}

func good() string {
	return `{"invocation_id":"` + inv + `","result":"success","exit_code":"exited","exit_status":"0"}`
}

func TestLegitimateReceiptStillWorks(t *testing.T) {
	r := newRig(t, 1)
	b := r.backend()
	startRunning(t, r, b, "a")
	r.receipt(0, inv, "success", "exited", "0")
	rc, err := b.WaitExit(ctx, "a")
	if err != nil || !rc.Clean {
		t.Fatalf("%v %+v", err, rc)
	}
}
