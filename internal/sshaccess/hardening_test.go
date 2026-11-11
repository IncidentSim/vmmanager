package sshaccess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuppliedGeneratedPublicStillConflicts(t *testing.T) {
	s, _, _ := newTestStore(t)
	pub, err := s.Ensure("vm-intent", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Ensure("vm-intent", pub.OwnerPublic); !errors.Is(err, ErrConflict) {
		t.Fatal("intent changed from generated to supplied", err)
	}
}
func TestPublicKeyRejectsCommentControls(t *testing.T) {
	_, pub, _ := GenerateEd25519()
	for _, c := range []string{"\x01", "\x1b", "\x7f"} {
		if _, err := ParsePublicKey(pub + " comment" + c); !errors.Is(err, ErrInvalidKey) {
			t.Fatal("accepted control", err)
		}
	}
}
func TestScrubPreservesUnownedMarkerAndMissingNewline(t *testing.T) {
	f := newFixture(t, stdPasswd)
	original := "# custom\nssh-ed25519 AAAAuser vibe-fc-managed:user"
	f.write("home/alice/.ssh/authorized_keys", original, 0600)
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-preserve", img, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Scrub(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if got != original {
		t.Fatalf("unowned bytes changed: %q", got)
	}
}
func TestScrubRefusesUnreadableAccount(t *testing.T) {
	f := newFixture(t, stdPasswd)
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-safe", img, ""); err != nil {
		t.Fatal(err)
	}
	g := d(p)
	if err := g.putFile(context.Background(), img, "/etc/passwd", []byte("changed:x:1:1::/different:/bin/sh\n"), true, 0644, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.Scrub(context.Background(), img); err == nil {
		t.Fatal("must not claim sanitization after losing account")
	}
}
func TestTerminalKeyRejectsUnsafeDirectory(t *testing.T) {
	s, _, _ := newTestStore(t)
	s.Ensure("vm-dir", "")
	dir := filepath.Join(t.TempDir(), "public")
	os.Mkdir(dir, 0755)
	if _, release, err := s.TerminalKeyFile("vm-dir", dir); err == nil {
		release()
		t.Fatal("accepted public runtime directory")
	}
}
func TestInstallRejectsSymlinkAncestor(t *testing.T) {
	f := newFixture(t, stdPasswd)
	f.mkdir("real/alice")
	os.Remove(filepath.Join(f.root, "home/alice"))
	os.Remove(filepath.Join(f.root, "home"))
	f.symlink("/real", "home")
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-sym", img, ""); !errors.Is(err, ErrUnsupportedImage) {
		t.Fatal("symlink ancestor accepted", err)
	}
}
func TestScrubKeepsPreexistingSuppliedKey(t *testing.T) {
	_, pub, _ := GenerateEd25519()
	original := pub + " existing-key\n"
	f := newFixture(t, stdPasswd)
	f.write("home/alice/.ssh/authorized_keys", original, 0600)
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-existing", img, pub); err != nil {
		t.Fatal(err)
	}
	if err := p.Scrub(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if got != original {
		t.Fatal(strings.TrimSpace(got))
	}
}
