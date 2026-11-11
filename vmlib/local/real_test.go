//go:build linux

package local_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib/local"
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
	if err := l.StartSlot(ctx, 3, 500); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if strings.TrimSpace(string(got)) != "start-slot\n3\n500" {
		t.Fatalf("args: %q", got)
	}
	for _, bad := range []int{-1, 64, 1000} {
		if err := l.StartSlot(ctx, bad, 500); err == nil {
			t.Errorf("slot %d accepted", bad)
		}
	}
	os.Remove(out)
	for _, q := range []uint32{0, 5, 505, 64010} {
		if err := l.StartSlot(ctx, 1, q); err == nil {
			t.Errorf("quota %d accepted", q)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("helper was executed for an invalid quota")
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
