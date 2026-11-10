//go:build linux

package local_test

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceParentRestoresVMMGroupTraversal(t *testing.T) {
	r := newRig(t, 2)
	parent := filepath.Join(r.root, "vms")
	// A daemon running with UMask=0077 creates this as 0700 even when
	// MkdirAll requests 0770. The root VMM has group access, not DAC override.
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	b := r.backend()
	if _, err := prep(b, "permissions"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{parent, filepath.Join(parent, "permissions")} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0770 {
			t.Errorf("%s mode=%#o, want 0770", path, st.Mode().Perm())
		}
	}
}
