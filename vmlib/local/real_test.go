//go:build linux

package local_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/firecracker-postgres-builder/vmlib/local"
)

// The launcher may pass nothing but a fixed verb and a bounded integer.
func TestHelperLauncherPassesOnlyFixedVerbAndSlot(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	helper := filepath.Join(dir, "helper.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := local.NewHelperLauncher(helper)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.StartSlot(ctx, 3); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "start-slot\n3" {
		t.Fatalf("args: %q", got)
	}
	for _, bad := range []int{-1, 64, 1000} {
		if err := l.StartSlot(ctx, bad); err == nil {
			t.Errorf("slot %d accepted", bad)
		}
	}
	if _, err := local.NewHelperLauncher("relative/helper"); err == nil {
		t.Error("relative helper path accepted")
	}
}

func TestSocketPathIsDerivedFromSlot(t *testing.T) {
	if got := local.SocketPath("/run/firecracker-vms", 7); got != "/run/firecracker-vms/slot-7/api.sock" {
		t.Fatal(got)
	}
}
