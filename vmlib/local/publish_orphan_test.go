//go:build linux

package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func TestPublishNeverConsumesOrphanIdentity(t *testing.T) {
	for _, failCopy := range []bool{false, true} {
		name := "successful-copy"
		if failCopy {
			name = "failed-copy"
		}
		t.Run(name, func(t *testing.T) {
			r := newPubRig(t)
			r.exited(t, "source", 0)
			marker := filepath.Join(r.cfg.ImagesDir, ".committed", "uncertain")
			if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
				t.Fatal(err)
			}
			evidence := []byte("unresolved prior publication or deletion")
			if err := os.WriteFile(marker, evidence, 0644); err != nil {
				t.Fatal(err)
			}
			if err := r.b.DeleteImage(context.Background(), "uncertain"); !errors.Is(err, vmlib.ErrNotStopped) {
				t.Fatalf("orphan deletion: %v", err)
			}
			r.failPublish.Store(failCopy)
			if _, err := r.b.Publish(context.Background(), "source", "uncertain"); !errors.Is(err, vmlib.ErrAlreadyExists) {
				t.Errorf("unresolved target must conflict before copy: %v", err)
			}
			got, err := os.ReadFile(marker)
			if err != nil || string(got) != string(evidence) {
				t.Fatalf("prior evidence modified: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(r.cfg.ImagesDir, "uncertain.ext4")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unresolved identity reused: %v", err)
			}
		})
	}
}
