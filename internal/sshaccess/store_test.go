package sshaccess

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	d := t.TempDir()
	keyFile := filepath.Join(d, "master.key")
	if err := GenerateMasterKeyFile(keyFile); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(d, "creds")
	s, err := OpenStore(dir, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, keyFile
}

func TestStoreGeneratedKeyIsStableAndEncrypted(t *testing.T) {
	s, dir, _ := newTestStore(t)
	a, err := s.Ensure("vm-1", "")
	if err != nil || !a.OwnerGenerated || a.OwnerPublic == "" || a.TerminalPublic == "" {
		t.Fatalf("%+v %v", a, err)
	}
	b, err := s.Ensure("vm-1", "") // replay
	if err != nil || b != a {
		t.Fatalf("replay must not regenerate: %+v vs %+v (%v)", b, a, err)
	}
	priv, err := s.OwnerPrivateKey("vm-1")
	if err != nil || !bytes.Contains(priv, []byte("BEGIN OPENSSH PRIVATE KEY")) {
		t.Fatalf("%v", err)
	}
	if a.OwnerPublic == a.TerminalPublic {
		t.Fatal("owner and terminal keys must differ")
	}
	// At rest nothing is plaintext.
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("want one credential file, got %d", len(ents))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ents[0].Name()))
	for _, secret := range []string{"OPENSSH", "PRIVATE", a.OwnerPublic, "ssh-ed25519"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("plaintext %q on disk", secret)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, ents[0].Name())); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
}

func TestStoreSuppliedKeyHasNoGeneratedPrivate(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, pub, _ := GenerateEd25519()
	c, err := s.Ensure("vm-2", pub+" me@host")
	if err != nil || c.OwnerGenerated || c.OwnerPublic != pub {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := s.OwnerPrivateKey("vm-2"); !errors.Is(err, ErrKeyNotGenerated) {
		t.Fatalf("want ErrKeyNotGenerated, got %v", err)
	}
	if _, err := s.Ensure("vm-2", pub); err != nil {
		t.Fatalf("same supplied key replay: %v", err)
	}
	_, other, _ := GenerateEd25519()
	if _, err := s.Ensure("vm-2", other); !errors.Is(err, ErrConflict) {
		t.Fatalf("different key for same VM must conflict, got %v", err)
	}
	if _, err := s.Ensure("vm-2", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("switching to generated must conflict, got %v", err)
	}
	if _, err := s.Ensure("vm-3", "garbage"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Public("vm-3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid key must not create a record: %v", err)
	}
}

func TestStoreGeneratedThenSuppliedConflicts(t *testing.T) {
	s, _, _ := newTestStore(t)
	s.Ensure("vm-4", "")
	_, pub, _ := GenerateEd25519()
	if _, err := s.Ensure("vm-4", pub); !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestStoreRejectsWrongMasterKeyTamperingAndSwap(t *testing.T) {
	s, dir, _ := newTestStore(t)
	s.Ensure("vm-a", "")
	s.Ensure("vm-b", "")
	// Wrong master key cannot read.
	d2 := t.TempDir()
	k2 := filepath.Join(d2, "other.key")
	GenerateMasterKeyFile(k2)
	s2, err := OpenStore(dir, k2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Public("vm-a"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("wrong key must fail closed: %v", err)
	}
	// Swapping records between VMs is detected (AAD binds the VM id).
	pa, pb := s.path("vm-a"), s.path("vm-b")
	ra, _ := os.ReadFile(pa)
	os.WriteFile(pb, ra, 0o600)
	if _, err := s.Public("vm-b"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("swap undetected: %v", err)
	}
	// Bit flip.
	ra[len(ra)-1] ^= 1
	os.WriteFile(pa, ra, 0o600)
	if _, err := s.OwnerPrivateKey("vm-a"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tamper undetected: %v", err)
	}
}

func TestStoreMasterKeyFileValidation(t *testing.T) {
	d := t.TempDir()
	good := filepath.Join(d, "k")
	GenerateMasterKeyFile(good)
	if err := GenerateMasterKeyFile(good); err == nil {
		t.Fatal("must not overwrite an existing key")
	}
	os.Chmod(good, 0o644)
	if _, err := OpenStore(filepath.Join(d, "c"), good); err == nil {
		t.Fatal("group/world readable master key accepted")
	}
	short := filepath.Join(d, "short")
	os.WriteFile(short, []byte("short"), 0o600)
	if _, err := OpenStore(filepath.Join(d, "c"), short); err == nil {
		t.Fatal("short master key accepted")
	}
	if _, err := OpenStore(filepath.Join(d, "c"), filepath.Join(d, "missing")); err == nil {
		t.Fatal("missing master key accepted")
	}
	// Symlinked key file is refused.
	GenerateMasterKeyFile(filepath.Join(d, "real"))
	os.Symlink(filepath.Join(d, "real"), filepath.Join(d, "link"))
	if _, err := OpenStore(filepath.Join(d, "c"), filepath.Join(d, "link")); err == nil {
		t.Fatal("symlinked master key accepted")
	}
}

func TestStoreDeleteAndIDValidation(t *testing.T) {
	s, _, _ := newTestStore(t)
	s.Ensure("vm-d", "")
	if err := s.Delete("vm-d"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("vm-d"); err != nil {
		t.Fatalf("delete must be idempotent: %v", err)
	}
	if _, err := s.OwnerPrivateKey("vm-d"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{"", "../x", "A", "a/b", strings.Repeat("a", 80)} {
		if _, err := s.Ensure(bad, ""); err == nil {
			t.Errorf("accepted id %q", bad)
		}
	}
}

func TestTerminalKeyFileMaterialisation(t *testing.T) {
	s, _, _ := newTestStore(t)
	c, _ := s.Ensure("vm-t", "")
	dir := filepath.Join(t.TempDir(), "term")
	path, release, err := s.TerminalKeyFile("vm-t", dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 || !filepath.IsAbs(path) {
		t.Fatalf("%v %v", fi, err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("OPENSSH PRIVATE KEY")) {
		t.Fatal("not a key")
	}
	p2, release2, err := s.TerminalKeyFile("vm-t", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if p2 == path {
		t.Fatal("leases must have unique paths")
	}
	release()
	release()
	if second, err := os.ReadFile(p2); err != nil || !bytes.Equal(second, data) {
		t.Fatal("release removed another lease", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("release must remove the plaintext key file")
	}
	// The terminal key is not the owner key.
	owner, _ := s.OwnerPrivateKey("vm-t")
	if bytes.Equal(owner, data) || c.TerminalPublic == c.OwnerPublic {
		t.Fatal("terminal key must be separate")
	}
}
