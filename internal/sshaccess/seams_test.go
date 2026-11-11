package sshaccess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestScrubVMReopenWithoutGuestDiscoveryMarker(t *testing.T) {
	f := newFixture(t, stdPasswd)
	original := "# notes\nwithout trailing newline"
	f.write("home/alice/.ssh/authorized_keys", original, 0600)
	img := f.image()
	s, dir, key := newTestStore(t)
	p := &Provisioner{Store: s, User: "alice"}
	if err := p.Install(context.Background(), "vm-reopen", img, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d(p).run(context.Background(), img, true, "rm "+receiptPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	p = &Provisioner{Store: reopened, User: "alice"}
	for i := 0; i < 2; i++ {
		if err = p.ScrubVM(context.Background(), "vm-reopen", img); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if got != original {
		t.Fatalf("%q", got)
	}
	fsck(t, img)
}

func TestScrubToolFailureAndSymlinkFailClosed(t *testing.T) {
	f := newFixture(t, stdPasswd)
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-tool", img, ""); err != nil {
		t.Fatal(err)
	}
	p.Debugfs = "/nonexistent/sshaccess-debugfs"
	if err := p.ScrubVM(context.Background(), "vm-tool", img); !errors.Is(err, ErrUnsupportedImage) {
		t.Fatal(err)
	}
	p.Debugfs = ""
	for _, cmd := range []string{"rm /home/alice/.ssh/authorized_keys", "symlink /home/alice/.ssh/authorized_keys /etc/passwd"} {
		if _, err := d(p).run(context.Background(), img, true, cmd); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.ScrubVM(context.Background(), "vm-tool", img); !errors.Is(err, ErrUnsupportedImage) {
		t.Fatal(err)
	}
	fsck(t, img)
}

func TestTerminalRejectsSymlinkAncestor(t *testing.T) {
	s, _, _ := newTestStore(t)
	if _, err := s.Ensure("vm-link", ""); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, release, err := s.TerminalKeyFile("vm-link", filepath.Join(link, "keys")); err == nil {
		release()
		t.Fatal("accepted symlink ancestor")
	}
}

func TestScrubExactOwnershipPreservesArbitraryUnmanagedBytes(t *testing.T) {
	f := newFixture(t, stdPasswd)
	original := "\x00# custom\r\n\n# vibe-fc-managed:created\nssh-ed25519 AAAAfake vibe-fc-managed:owner:not-ours"
	f.write("home/alice/.ssh/authorized_keys", original, 0600)
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-bytes", img, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.ScrubVM(context.Background(), "vm-bytes", img); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if got != original {
		t.Fatalf("%q", got)
	}
	fsck(t, img)
}

func TestConcurrentTerminalLeases(t *testing.T) {
	s, _, _ := newTestStore(t)
	if _, err := s.Ensure("vm-parallel", ""); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	paths := map[string]bool{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, release, err := s.TerminalKeyFile("vm-parallel", dir)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			mu.Lock()
			defer mu.Unlock()
			if paths[path] {
				t.Error("reused lease path")
			}
			paths[path] = true
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "OPENSSH PRIVATE KEY") {
				t.Error("invalid lease", err)
			}
		}()
	}
	wg.Wait()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("leaked lease", err)
	}
}

func TestPreexistingExactOwnedLookingLinePreserved(t *testing.T) {
	p, s := newProv(t, "alice")
	pub, err := s.Ensure("vm-looking", "")
	if err != nil {
		t.Fatal(err)
	}
	original := pub.OwnerPublic + " " + markerPrefix + "owner:vm-looking\n"
	f := newFixture(t, stdPasswd)
	f.write("home/alice/.ssh/authorized_keys", original, 0600)
	img := f.image()
	if err := p.Install(context.Background(), "vm-looking", img, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.ScrubVM(context.Background(), "vm-looking", img); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if got != original {
		t.Fatal(got)
	}
	fsck(t, img)
}

func TestModifiedCopiesAreNotGenericErasure(t *testing.T) {
	f := newFixture(t, stdPasswd)
	img := f.image()
	p, s := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-copy", img, ""); err != nil {
		t.Fatal(err)
	}
	pub, _ := s.Public("vm-copy")
	// Guest deliberately copied key without our ownership comment. This must not
	// be mistaken for a managed record: it could be a legitimate preexisting key.
	if err := guestAppend(t, img, "/home/alice/.ssh/authorized_keys", pub.OwnerPublic+" guest-copy\n"); err != nil {
		t.Fatal(err)
	}
	if err := p.ScrubVM(context.Background(), "vm-copy", img); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if got != pub.OwnerPublic+" guest-copy\n" || strings.Contains(got, "vibe-fc-managed:") {
		t.Fatal(got)
	}
	fsck(t, img)
}
