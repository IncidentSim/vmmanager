package hostinstall

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReinstallPreservesOmittedAssets(t *testing.T) {
	o := staged(t)
	o.Firecracker = filepath.Join(o.BuildDir, "vm-launcher")
	o.Kernel = filepath.Join(t.TempDir(), "kernel")
	if err := os.WriteFile(o.Kernel, []byte("kernel fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	o.Firecracker, o.Kernel = "", ""
	m, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"firecracker", "vmlinux"} {
		p := o.withDefaults().AssetDir + "/" + name
		if _, err := os.Stat(filepath.Join(o.DestDir, p)); err != nil {
			t.Fatalf("omitted asset removed: %s: %v", name, err)
		}
		found := false
		for _, f := range m.Files {
			found = found || f.Path == p
		}
		if !found {
			t.Errorf("asset %s lost manifest ownership", name)
		}
	}
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"firecracker", "vmlinux"} {
		if _, err := os.Stat(filepath.Join(o.DestDir, o.withDefaults().AssetDir, name)); !os.IsNotExist(err) {
			t.Errorf("asset not uninstalled: %s: %v", name, err)
		}
	}
}

func TestSetupIncludesNetworkRuntime(t *testing.T) {
	o := staged(t)
	var out strings.Builder
	// Dry-run needs an existing non-root user; the current test process is unprivileged.
	u := os.Getenv("USER")
	if u == "" || u == "root" {
		u = "nobody"
	}
	if err := Setup(context.Background(), SetupOptions{Options: o, User: u, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "/run/firecracker-vms-net 0755") {
		t.Fatalf("network runtime missing: %s", out.String())
	}
	text, err := readEmbedded("multivm/firecracker-vms.tmpfiles.conf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "d     /run/firecracker-vms-net") {
		t.Fatal("network runtime not recreated at boot")
	}
}
