package vmnet

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExecRunnerIgnoresInheritedPATH(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "ip"), []byte("#!/bin/sh\nprintf attacker-selected"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d)
	r, err := (ExecRunner{}).Run(context.Background(), "", "ip", "-Version")
	if err != nil {
		t.Fatal(err)
	}
	if r.Stdout == "attacker-selected" {
		t.Fatal("executed inherited PATH stub")
	}
}
