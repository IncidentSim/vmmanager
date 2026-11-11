//go:build linux

package local_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// A persisted AccessManaged bit, not global configuration, credential presence,
// request digests or TTL metadata, determines each VM's authentication mode.
func TestSSHAuthModeSurvivesPersonalAccessActivation(t *testing.T) {
	r, store := realAccessRig(t)
	access := r.cfg.Access
	r.cfg.Access = nil
	b := r.backend()
	if _, err := prep(b, "legacy-running"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(ctx, "legacy-running"); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(r.root, "vms/legacy-running/disk.ext4")
	before := sum(t, disk)
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	r.cfg.Access = access
	b = r.backend()
	svc, err := vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	info, err := svc.SSHInfo(ctx, "legacy-running")
	if err != nil {
		t.Fatal(err)
	}
	if info.AuthMode != vmlib.AuthImagePreinstalled || info.PublicKeyProvisioned {
		t.Fatalf("legacy guest reclassified by global access config: %+v", info)
	}
	if _, err := store.Public("legacy-running"); !errors.Is(err, sshaccess.ErrNotFound) {
		t.Fatalf("legacy credentials unexpectedly created: %v", err)
	}
	if sum(t, disk) != before {
		t.Fatal("activation changed running legacy disk")
	}
	if _, err := prep(b, "managed-missing"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("managed-missing"); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// Even losing the provisioning configuration cannot turn a managed VM legacy.
	r.cfg.Access = nil
	b = r.backend()
	svc, err = vmlib.NewService(b)
	if err != nil {
		t.Fatal(err)
	}
	info, err = svc.SSHInfo(ctx, "managed-missing")
	if err != nil {
		t.Fatal(err)
	}
	if info.AuthMode != vmlib.AuthProvisionPublicKey {
		t.Fatalf("missing managed key downgraded: %+v", info)
	}
}
