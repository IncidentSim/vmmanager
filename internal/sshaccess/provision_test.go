package sshaccess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needTools(t *testing.T) {
	t.Helper()
	for _, b := range []string{"mkfs.ext4", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(b); err != nil {
			if p := "/usr/sbin/" + b; fileExists(p) {
				continue
			}
			t.Skipf("%s not installed", b)
		}
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// fixture builds a real, disposable ext4 image from a directory tree with
// mkfs.ext4 -d (no root, no mount).
type fixture struct {
	t    *testing.T
	root string
}

func newFixture(t *testing.T, passwd string) *fixture {
	needTools(t)
	f := &fixture{t: t, root: t.TempDir()}
	f.mkdir("etc")
	f.mkdir("root")
	f.mkdir("home/alice")
	f.write("etc/passwd", passwd, 0o644)
	return f
}

const stdPasswd = "root:x:0:0:root:/root:/bin/bash\nalice:x:1234:2345::/home/alice:/bin/sh\n"

func (f *fixture) mkdir(p string) {
	if err := os.MkdirAll(filepath.Join(f.root, p), 0o755); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) write(p, content string, mode os.FileMode) {
	f.mkdir(filepath.Dir(p))
	fp := filepath.Join(f.root, p)
	if err := os.WriteFile(fp, []byte(content), mode); err != nil {
		f.t.Fatal(err)
	}
	os.Chmod(fp, mode)
}
func (f *fixture) symlink(target, p string) {
	f.mkdir(filepath.Dir(p))
	if err := os.Symlink(target, filepath.Join(f.root, p)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) image() string {
	img := filepath.Join(f.t.TempDir(), "disk.ext4")
	out, err := exec.Command(lookTool("mkfs.ext4"), "-q", "-F", "-d", f.root, img, "16M").CombinedOutput()
	if err != nil {
		f.t.Fatalf("mkfs: %v %s", err, out)
	}
	return img
}

func lookTool(n string) string {
	if p, err := exec.LookPath(n); err == nil {
		return p
	}
	return "/usr/sbin/" + n
}

func dbg(t *testing.T, img, cmd string) string {
	t.Helper()
	out, err := exec.Command(lookTool("debugfs"), "-R", cmd, img).CombinedOutput()
	if err != nil {
		t.Fatalf("debugfs %s: %v %s", cmd, err, out)
	}
	return string(out)
}

func readGuest(t *testing.T, img, p string) (string, bool) {
	t.Helper()
	out := dbg(t, img, "cat "+p)
	if strings.Contains(out, "File not found") {
		return "", false
	}
	return strings.TrimPrefix(out[strings.Index(out, "\n")+1:], ""), true
}

func fsck(t *testing.T, img string) {
	t.Helper()
	out, err := exec.Command(lookTool("e2fsck"), "-fn", img).CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck reports corruption: %v\n%s", err, out)
	}
}

func newProv(t *testing.T, user string) (*Provisioner, *Store) {
	s, _, _ := newTestStore(t)
	return &Provisioner{Store: s, User: user}, s
}

func TestInstallCreatesKeysOnFreshHome(t *testing.T) {
	f := newFixture(t, stdPasswd)
	img := f.image()
	p, s := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-1", img, ""); err != nil {
		t.Fatal(err)
	}
	pub, _ := s.Public("vm-1")
	got, ok := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if !ok {
		t.Fatal("authorized_keys missing")
	}
	if !strings.Contains(got, pub.OwnerPublic) || !strings.Contains(got, pub.TerminalPublic) {
		t.Fatalf("both owner and terminal key must be installed:\n%s", got)
	}
	if strings.Contains(got, "PRIVATE") {
		t.Fatal("private material on disk")
	}
	st := dbg(t, img, "stat /home/alice/.ssh/authorized_keys")
	if !strings.Contains(st, "Mode:  0600") || !strings.Contains(st, "User:  1234") || !strings.Contains(st, "Group:  2345") {
		t.Fatalf("sshd StrictModes needs 0600 and user ownership:\n%s", st)
	}
	st = dbg(t, img, "stat /home/alice/.ssh")
	if !strings.Contains(st, "Mode:  0700") || !strings.Contains(st, "User:  1234") {
		t.Fatalf(".ssh dir:\n%s", st)
	}
	fsck(t, img)
}

func TestInstallSuppliedKeyInstallsOnlyThatKeyForOwner(t *testing.T) {
	f := newFixture(t, stdPasswd)
	img := f.image()
	p, s := newProv(t, "root")
	_, mine, _ := GenerateEd25519()
	if err := p.Install(context.Background(), "vm-2", img, mine+" me@laptop"); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/root/.ssh/authorized_keys")
	pub, _ := s.Public("vm-2")
	if !strings.Contains(got, mine+" vibe-fc-managed:owner") || !strings.Contains(got, pub.TerminalPublic) {
		t.Fatalf("%s", got)
	}
	if strings.Contains(got, "me@laptop") {
		t.Fatal("comment should be dropped")
	}
	fsck(t, img)
}

func TestInstallIsIdempotentAndPreservesExistingKeys(t *testing.T) {
	legit := "# my notes\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILEGITLEGITLEGITLEGITLEGITLEGITLEGITLEGITLEGI legit@corp\n"
	f := newFixture(t, stdPasswd)
	f.write("home/alice/.ssh/authorized_keys", legit, 0o600)
	f.write("home/alice/.ssh/known_hosts", "h ssh-ed25519 AAAA\n", 0o644)
	img := f.image()
	p, _ := newProv(t, "alice")
	for i := 0; i < 2; i++ {
		if err := p.Install(context.Background(), "vm-3", img, ""); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if !strings.HasPrefix(got, legit) {
		t.Fatalf("legit content lost:\n%s", got)
	}
	if n := strings.Count(got, "vibe-fc-managed:owner"); n != 1 {
		t.Fatalf("install must be idempotent, owner lines=%d\n%s", n, got)
	}
	if kh, ok := readGuest(t, img, "/home/alice/.ssh/known_hosts"); !ok || !strings.Contains(kh, "h ssh-ed25519") {
		t.Fatal("sibling files must be untouched")
	}
	fsck(t, img)
}

func TestInstallHandlesMissingTrailingNewline(t *testing.T) {
	f := newFixture(t, stdPasswd)
	f.write("home/alice/.ssh/authorized_keys", "ssh-ed25519 AAAAlegit nonl", 0o600)
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-nl", img, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if !strings.HasPrefix(got, "ssh-ed25519 AAAAlegit nonl\nssh-ed25519 ") {
		t.Fatalf("lines must not be glued together:\n%s", got)
	}
}

func TestScrubRestoresExactlyWhatWasThere(t *testing.T) {
	legit := "# notes\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAILEGITLEGITLEGITLEGITLEGITLEGITLEGITLEGITLEGI legit@corp\n\nfrom=\"10.0.0.0/8\" ssh-ed25519 AAAAother opts\n"
	f := newFixture(t, stdPasswd)
	f.write("home/alice/.ssh/authorized_keys", legit, 0o600)
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-4", img, ""); err != nil {
		t.Fatal(err)
	}
	// Guest-side legitimate change after boot must survive Commit.
	if err := guestAppend(t, img, "/home/alice/.ssh/authorized_keys", "ssh-ed25519 AAAAadded added-by-user\n"); err != nil {
		t.Fatal(err)
	}
	if err := p.Scrub(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	want := legit + "ssh-ed25519 AAAAadded added-by-user\n"
	if got != want {
		t.Fatalf("scrub must remove only managed lines.\nwant:\n%q\ngot:\n%q", want, got)
	}
	if strings.Contains(got, "vibe-fc-managed") {
		t.Fatal("managed marker survived")
	}
	// Scrub is idempotent.
	if err := p.Scrub(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	fsck(t, img)
}

func guestAppend(t *testing.T, img, path, extra string) error {
	t.Helper()
	cur, _ := readGuest(t, img, path)
	tmp := filepath.Join(t.TempDir(), "x")
	os.WriteFile(tmp, []byte(cur+extra), 0o600)
	for _, c := range []string{"rm " + path, "write " + tmp + " " + path, "sif " + path + " mode 0100600"} {
		out, err := exec.Command(lookTool("debugfs"), "-w", "-R", c, img).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v %s", err, out)
		}
	}
	return nil
}

func TestScrubRemovesFileWeCreated(t *testing.T) {
	f := newFixture(t, stdPasswd)
	img := f.image()
	p, _ := newProv(t, "alice")
	p.Install(context.Background(), "vm-5", img, "")
	if err := p.Scrub(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	if _, ok := readGuest(t, img, "/home/alice/.ssh/authorized_keys"); ok {
		t.Fatal("file created solely for managed keys must not be published")
	}
	fsck(t, img)
}

func TestScrubKeepsFileWhenGuestAddedContentToCreatedFile(t *testing.T) {
	f := newFixture(t, stdPasswd)
	img := f.image()
	p, _ := newProv(t, "alice")
	p.Install(context.Background(), "vm-6", img, "")
	guestAppend(t, img, "/home/alice/.ssh/authorized_keys", "ssh-ed25519 AAAAmine mine\n")
	if err := p.Scrub(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	got, _ := readGuest(t, img, "/home/alice/.ssh/authorized_keys")
	if strings.TrimSpace(got) != "ssh-ed25519 AAAAmine mine" {
		t.Fatalf("%q", got)
	}
}

func TestScrubOnImageWithoutAccessIsNoop(t *testing.T) {
	f := newFixture(t, stdPasswd)
	img := f.image()
	before, _ := os.ReadFile(img)
	p, _ := newProv(t, "alice")
	if err := p.Scrub(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(img)
	if !bytes.Equal(before, after) {
		t.Fatal("scrub of a clean image must not write")
	}
}

func TestInstallRefusesUnsupportedImages(t *testing.T) {
	ctx := context.Background()
	t.Run("not ext4", func(t *testing.T) {
		needTools(t)
		img := filepath.Join(t.TempDir(), "raw")
		os.WriteFile(img, bytes.Repeat([]byte("x"), 1<<20), 0o600)
		p, _ := newProv(t, "root")
		if err := p.Install(ctx, "vm-x", img, ""); !errors.Is(err, ErrUnsupportedImage) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unknown user", func(t *testing.T) {
		f := newFixture(t, stdPasswd)
		p, _ := newProv(t, "bob")
		if err := p.Install(ctx, "vm-x", f.image(), ""); !errors.Is(err, ErrUnsupportedImage) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("home missing", func(t *testing.T) {
		f := newFixture(t, "ghost:x:5:5::/home/ghost:/bin/sh\n")
		p, _ := newProv(t, "ghost")
		if err := p.Install(ctx, "vm-x", f.image(), ""); !errors.Is(err, ErrUnsupportedImage) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("hostile home path", func(t *testing.T) {
		f := newFixture(t, "evil:x:5:5::/home/a b;quit:/bin/sh\n")
		p, _ := newProv(t, "evil")
		if err := p.Install(ctx, "vm-x", f.image(), ""); !errors.Is(err, ErrUnsupportedImage) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("authorized_keys is a symlink", func(t *testing.T) {
		f := newFixture(t, stdPasswd)
		f.mkdir("home/alice/.ssh")
		f.symlink("/etc/passwd", "home/alice/.ssh/authorized_keys")
		img := f.image()
		p, _ := newProv(t, "alice")
		if err := p.Install(ctx, "vm-x", img, ""); !errors.Is(err, ErrUnsupportedImage) {
			t.Fatalf("got %v", err)
		}
		if pw, _ := readGuest(t, img, "/etc/passwd"); pw != stdPasswd {
			t.Fatal("symlink target modified")
		}
	})
	t.Run(".ssh is a symlink", func(t *testing.T) {
		f := newFixture(t, stdPasswd)
		f.mkdir("elsewhere")
		f.symlink("/elsewhere", "home/alice/.ssh")
		p, _ := newProv(t, "alice")
		if err := p.Install(ctx, "vm-x", f.image(), ""); !errors.Is(err, ErrUnsupportedImage) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("invalid supplied key", func(t *testing.T) {
		f := newFixture(t, stdPasswd)
		p, _ := newProv(t, "alice")
		if err := p.Install(ctx, "vm-x", f.image(), "nonsense"); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestInstallLeavesOtherGuestContentUntouched(t *testing.T) {
	f := newFixture(t, stdPasswd)
	f.write("etc/ssh/sshd_config", "PasswordAuthentication no\n", 0o644)
	f.write("usr/bin/tool", "#!/bin/sh\n", 0o755)
	img := f.image()
	p, _ := newProv(t, "alice")
	if err := p.Install(context.Background(), "vm-7", img, ""); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/etc/ssh/sshd_config": "PasswordAuthentication no\n", "/usr/bin/tool": "#!/bin/sh\n", "/etc/passwd": stdPasswd} {
		if got, _ := readGuest(t, img, path); got != want {
			t.Fatalf("%s changed: %q", path, got)
		}
	}
}

func TestInstallHonoursContextCancellation(t *testing.T) {
	f := newFixture(t, stdPasswd)
	p, _ := newProv(t, "alice")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Install(ctx, "vm-c", f.image(), ""); err == nil {
		t.Fatal("cancelled context must fail")
	}
}
