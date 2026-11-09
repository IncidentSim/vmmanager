package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInvocationIDUsedByFirecracker(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	args, err := launcherArgs(id)
	if err != nil || len(args) != 5 || args[3] != "--id" || args[4] != id {
		t.Fatalf("args=%q error=%v", args, err)
	}
	for _, bad := range []string{"", "../bad", "0123456789ABCDEF0123456789ABCDEF"} {
		if _, err := launcherArgs(bad); err == nil {
			t.Fatalf("accepted invalid invocation: %q", bad)
		}
	}
}

func TestExitReceiptIsAtomicAndImmutable(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	if err := recordExit(dir, id, "success", "exited", "0"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".exit.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["invocation_id"] != id || fields["result"] != "success" || fields["exit_code"] != "exited" || fields["exit_status"] != "0" {
		t.Fatalf("wrong receipt: %v", fields)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0640 {
		t.Fatalf("permissions: %v", info.Mode())
	}
	if err := recordExit(dir, id, "signal", "killed", "TERM"); err == nil {
		t.Fatal("overwrote an existing receipt")
	}
	if err := recordExit(dir, "../escape", "success", "exited", "0"); err == nil {
		t.Fatal("accepted path traversal")
	}
}
