//go:build linux

package local_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func TestRealSSHUnsupportedGuestPreparationRetainsCredentialUntilDelete(t *testing.T) {
	r, s := realAccessRig(t)
	base := filepath.Join(r.images, "base.ext4")
	baseHash := sum(t, base)
	unsupported := filepath.Join(r.images, "unsupported.ext4")
	if out, err := exec.Command("cp", "--", base, unsupported).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v %s", err, out)
	}
	if err := os.Chmod(unsupported, 0600); err != nil {
		t.Fatal(err)
	}
	accessCommand(t, "debugfs", "-w", "-R", "rm /etc/passwd", unsupported)
	if err := os.Chmod(unsupported, 0444); err != nil {
		t.Fatal(err)
	}
	unsupportedHash := sum(t, unsupported)
	b := r.backend()
	_, err := b.Prepare(ctx, vmlib.PrepareRequest{VMID: "unsupported-vm", ImageID: "unsupported", Profile: "small"})
	if err == nil {
		t.Fatal("unsupported guest accepted")
	}
	if _, err = b.Start(ctx, "unsupported-vm"); err == nil {
		t.Fatal("failed install booted")
	}
	if _, err = s.OwnerPrivateKey("unsupported-vm"); err != nil {
		t.Fatal("failed prepare erased credentials before Delete", err)
	}
	if _, err = os.Stat(filepath.Join(r.root, "vms/unsupported-vm")); !os.IsNotExist(err) {
		t.Fatal("failed workspace retained", err)
	}
	if sum(t, base) != baseHash || sum(t, unsupported) != unsupportedHash {
		t.Fatal("source image changed")
	}
	if result, err := b.Release(ctx, "unsupported-vm"); err != nil || !result.Released {
		t.Fatal("delete failed", err)
	}
	if _, err = s.Public("unsupported-vm"); !errors.Is(err, sshaccess.ErrNotFound) {
		t.Fatal("confirmed absent-workspace delete did not clean credentials", err)
	}
}

func TestRealSSHFailedWorkspaceDeleteRetainsCredentials(t *testing.T) {
	r, s := realAccessRig(t)
	b := r.backend()
	if _, err := prep(b, "delete-fails"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.root, "vms/delete-fails")
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0770)
	result, err := b.Release(ctx, "delete-fails")
	if err == nil || result.Released {
		t.Fatalf("failed removal accepted: %v %v", result, err)
	}
	if _, err = s.OwnerPrivateKey("delete-fails"); err != nil {
		t.Fatal("failed delete lost credentials", err)
	}
	if err = os.Chmod(dir, 0770); err != nil {
		t.Fatal(err)
	}
	if result, err = b.Release(ctx, "delete-fails"); err != nil || !result.Released {
		t.Fatalf("retry: %v %v", result, err)
	}
	if _, err = s.Public("delete-fails"); !errors.Is(err, sshaccess.ErrNotFound) {
		t.Fatal("confirmed deletion did not clean credentials", err)
	}
}

func TestRealSSHRestartReplayAndMissingScrubberFailsClosed(t *testing.T) {
	r, s := realAccessRig(t)
	b := r.backend()
	if _, err := prep(b, "restart-key"); err != nil {
		t.Fatal(err)
	}
	before, err := s.OwnerPrivateKey("restart-key")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	b = r.backend()
	if _, err = prep(b, "restart-key"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.OwnerPrivateKey("restart-key")
	if string(before) != string(after) {
		t.Fatal("restart regenerated credentials")
	}
	_, pub, err := sshaccess.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Prepare(ctx, vmlib.PrepareRequest{VMID: "restart-key", ImageID: "base", Profile: "small", SSHPublicKey: pub}); !errors.Is(err, vmlib.ErrAlreadyExists) {
		t.Fatal("changed access intent silently accepted", err)
	}
	if _, err = b.Start(ctx, "restart-key"); err != nil {
		t.Fatal(err)
	}
	r.receipt(0, inv, "success", "exited", "0")
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	r.cfg.Access = nil
	b = r.backend()
	if _, err = b.Publish(ctx, "restart-key", "unscrubbed"); err == nil {
		t.Fatal("managed VM published without scrubber")
	}
	if _, err = os.Stat(filepath.Join(r.images, "unscrubbed.ext4")); !os.IsNotExist(err) {
		t.Fatal("unscrubbed output visible", err)
	}
}

func TestRealSSHSuppliedOwnerKeyAndDedicatedTerminal(t *testing.T) {
	r, s := realAccessRig(t)
	b := r.backend()
	_, public, err := sshaccess.GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Prepare(ctx, vmlib.PrepareRequest{VMID: "supplied-owner", ImageID: "base", Profile: "small", SSHPublicKey: public})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Public("supplied-owner")
	if err != nil {
		t.Fatal(err)
	}
	if p.OwnerPublic != public || p.OwnerGenerated || p.TerminalPublic == public {
		t.Fatal("supplied owner/terminal identities wrong")
	}
	if _, err = s.OwnerPrivateKey("supplied-owner"); !errors.Is(err, sshaccess.ErrKeyNotGenerated) {
		t.Fatal("supplied key falsely has private half", err)
	}
	got := guestAccess(t, filepath.Join(r.root, "vms/supplied-owner/disk.ext4"))
	if !strings.Contains(got, p.OwnerPublic) || !strings.Contains(got, p.TerminalPublic) {
		t.Fatal("supplied + terminal key not installed")
	}
}
