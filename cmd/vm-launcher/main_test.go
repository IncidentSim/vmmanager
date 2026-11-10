package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

const id = "0123456789abcdef0123456789abcdef"

func TestParseSlotAcceptsOnlyBoundedDecimal(t *testing.T) {
	for in, want := range map[string]int{"0": 0, "7": 7, "63": 63} {
		if got, err := parseSlot(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "-1", "64", "007", "1; reboot", "../1", " 1", "1 ", "0x1", "١"} {
		if _, err := parseSlot(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestLauncherArgsUseInvocationIDAndFixedSocket(t *testing.T) {
	args, err := launcherArgs(id)
	if err != nil || len(args) != 5 || args[2] != "/run/slot/api.sock" || args[4] != id {
		t.Fatalf("%q %v", args, err)
	}
	for _, bad := range []string{"", "../x", "0123456789ABCDEF0123456789ABCDEF"} {
		if _, err := launcherArgs(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestReceiptIsAtomicImmutableAndValidated(t *testing.T) {
	dir := t.TempDir()
	if err := recordExit(dir, id, "success", "exited", "0"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, id+".exit.json"))
	var f map[string]string
	if err := json.Unmarshal(data, &f); err != nil || f["invocation_id"] != id || f["exit_status"] != "0" {
		t.Fatalf("%v %v", f, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, id+".exit.json")); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", info.Mode())
	}
	if recordExit(dir, id, "signal", "killed", "9") == nil {
		t.Fatal("receipt overwritten")
	}
	if recordExit(dir, "../escape", "success", "exited", "0") == nil {
		t.Fatal("traversal accepted")
	}
}

func TestStaleSocketCleanupRefusesLiveAndNonSocket(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "regular")
	os.WriteFile(reg, nil, 0o600)
	if cleanupStaleSocket(reg) == nil {
		t.Fatal("removed a non-socket")
	}
	live := filepath.Join(dir, "live.sock")
	l, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if cleanupStaleSocket(live) == nil {
		t.Fatal("removed a live socket")
	}
	if cleanupStaleSocket(filepath.Join(dir, "absent")) != nil {
		t.Fatal("absent path must be fine")
	}
}
