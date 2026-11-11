//go:build linux

package local_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type diskAccess struct {
	*sshaccess.Provisioner
	store *sshaccess.Store
}

func (a diskAccess) Delete(id string) error { return a.store.Delete(id) }

func accessTool(t *testing.T, n string) string {
	t.Helper()
	if p, e := exec.LookPath(n); e == nil {
		return p
	}
	if _, e := os.Stat("/usr/sbin/" + n); e == nil {
		return "/usr/sbin/" + n
	}
	t.Fatalf("required real filesystem tool %s unavailable", n)
	return ""
}
func accessCommand(t *testing.T, n string, args ...string) string {
	t.Helper()
	out, err := exec.Command(accessTool(t, n), args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", n, err, out)
	}
	return string(out)
}
func realAccessRig(t *testing.T) (*rig, *sshaccess.Store) {
	t.Helper()
	r := newRig(t, 3)
	tree := t.TempDir()
	for _, p := range []string{"etc", "root/.ssh"} {
		if err := os.MkdirAll(filepath.Join(tree, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "etc/passwd"), []byte("root:x:0:0::/root:/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "root/.ssh/authorized_keys"), []byte("legitimate image bytes without newline"), 0600); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(r.images, "base.ext4")
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	accessCommand(t, "mkfs.ext4", "-q", "-F", "-d", tree, base, "16M")
	if err := os.Chmod(base, 0444); err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	key := filepath.Join(d, "master")
	if err := sshaccess.GenerateMasterKeyFile(key); err != nil {
		t.Fatal(err)
	}
	s, err := sshaccess.OpenStore(filepath.Join(d, "credentials"), key)
	if err != nil {
		t.Fatal(err)
	}
	r.cfg.Access = diskAccess{&sshaccess.Provisioner{Store: s, User: "root", Debugfs: accessTool(t, "debugfs")}, s}
	return r, s
}
func guestAccess(t *testing.T, disk string) string {
	t.Helper()
	out := accessCommand(t, "debugfs", "-R", "cat /root/.ssh/authorized_keys", disk)
	return out[strings.Index(out, "\n")+1:]
}

func TestRealSSHPrepareCommitDerivative(t *testing.T) {
	r, s := realAccessRig(t)
	b := r.backend()
	base := filepath.Join(r.images, "base.ext4")
	baseHash := sum(t, base)
	h, err := prep(b, "ssh-parent")
	if err != nil {
		t.Fatal(err)
	}
	if !h.PublicKeyProvisioned {
		t.Fatal("real installation not advertised")
	}
	caps, _ := b.Capabilities(ctx)
	if caps.SSHAuthMode != vmlib.AuthProvisionPublicKey {
		t.Fatal(caps)
	}
	p, err := s.Public("ssh-parent")
	if err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(r.root, "vms/ssh-parent/disk.ext4")
	got := guestAccess(t, disk)
	if !strings.Contains(got, p.OwnerPublic) || !strings.Contains(got, p.TerminalPublic) {
		t.Fatal("real owner + dedicated terminal keys missing")
	}
	owner, err := s.OwnerPrivateKey("ssh-parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prep(b, "ssh-parent"); err != nil {
		t.Fatal(err)
	}
	replay, _ := s.OwnerPrivateKey("ssh-parent")
	if string(replay) != string(owner) {
		t.Fatal("replay changed key")
	}
	if _, err = b.Start(ctx, "ssh-parent"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Publish(ctx, "ssh-parent", "premature"); !errors.Is(err, vmlib.ErrNotStopped) {
		t.Fatalf("missing exit proof accepted: %v", err)
	}
	r.receipt(0, inv, "success", "exited", "0")
	before := sum(t, disk)
	svc, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Commit(ctx, "ssh-parent", "clean-ssh"); err != nil {
		t.Fatal(err)
	}
	if sum(t, disk) != before || sum(t, base) != baseHash {
		t.Fatal("immutable source/base changed")
	}
	published := filepath.Join(r.images, "clean-ssh.ext4")
	if got := guestAccess(t, published); got != "legitimate image bytes without newline" {
		t.Fatalf("publication hygiene: %q", got)
	}
	accessCommand(t, "e2fsck", "-fn", published)
	_, err = b.Prepare(ctx, vmlib.PrepareRequest{VMID: "ssh-child", ImageID: "clean-ssh", Profile: "small"})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := s.Public("ssh-child")
	if err != nil {
		t.Fatal(err)
	}
	child := guestAccess(t, filepath.Join(r.root, "vms/ssh-child/disk.ext4"))
	if !strings.Contains(child, cp.OwnerPublic) || strings.Contains(child, p.OwnerPublic) || cp.OwnerPublic == p.OwnerPublic {
		t.Fatal("derivative inherited temporary parent keys")
	}
	if _, err = s.OwnerPrivateKey("ssh-parent"); err != nil {
		t.Fatal("commit prematurely removed credentials")
	}
	if res, err := b.Release(ctx, "ssh-parent"); err != nil || !res.Released {
		t.Fatalf("release: %v %v", res, err)
	}
	if _, err = s.Public("ssh-parent"); !errors.Is(err, sshaccess.ErrNotFound) {
		t.Fatal("confirmed delete retained credentials", err)
	}
}

func TestRealSSHFailedScrubRetainsSourceAndPublishesNothing(t *testing.T) {
	r, s := realAccessRig(t)
	b := r.backend()
	baseHash := sum(t, filepath.Join(r.images, "base.ext4"))
	if _, err := prep(b, "bad-scrub"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(ctx, "bad-scrub"); err != nil {
		t.Fatal(err)
	}
	if result, err := b.Release(ctx, "bad-scrub"); err != nil || result.Released {
		t.Fatalf("uncertain delete: %v %v", result, err)
	}
	if _, err := s.OwnerPrivateKey("bad-scrub"); err != nil {
		t.Fatal("uncertain delete lost credentials", err)
	}
	r.receipt(0, inv, "success", "exited", "0")
	disk := filepath.Join(r.root, "vms/bad-scrub/disk.ext4")
	accessCommand(t, "debugfs", "-w", "-R", "rm /etc/passwd", disk)
	before := sum(t, disk)
	if _, err := b.Publish(context.Background(), "bad-scrub", "must-not-exist"); err == nil {
		t.Fatal("unsupported scrub published")
	}
	if _, err := os.Stat(filepath.Join(r.images, "must-not-exist.ext4")); !os.IsNotExist(err) {
		t.Fatal("failed output visible", err)
	}
	if _, err := os.Stat(filepath.Join(r.images, ".committed/must-not-exist")); !os.IsNotExist(err) {
		t.Fatal("failed marker retained", err)
	}
	temps, _ := filepath.Glob(filepath.Join(r.images, ".publish-*"))
	if len(temps) != 0 {
		t.Fatal("failed copies retained", temps)
	}
	if sum(t, disk) != before || sum(t, filepath.Join(r.images, "base.ext4")) != baseHash {
		t.Fatal("scrub mutated source/base")
	}
	if _, err := s.Public("bad-scrub"); err != nil {
		t.Fatal("failed scrub lost credentials", err)
	}
}

func TestRealSSHInstallFailureNeverBootsOrChangesBase(t *testing.T) {
	r, s := realAccessRig(t)
	base := filepath.Join(r.images, "base.ext4")
	before := sum(t, base)
	// Invalid supplied key exercises the real provisioner and fails closed.
	b := r.backend()
	_, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "bad-key", ImageID: "base", Profile: "small", SSHPublicKey: "not-a-key"})
	if err == nil {
		t.Fatal("invalid install accepted")
	}
	if _, err = b.Start(ctx, "bad-key"); err == nil {
		t.Fatal("failed preparation can boot")
	}
	if sum(t, base) != before {
		t.Fatal("base changed")
	}
	if _, err = s.Public("bad-key"); !errors.Is(err, sshaccess.ErrNotFound) {
		t.Fatal(err)
	}
}
