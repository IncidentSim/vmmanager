package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveOnlyStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.sock")
	if err := cleanupStaleSocket(path); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := cleanupStaleSocket(path); err == nil {
		t.Fatal("must not unlink a live socket")
	}
	listener.Close()
	if err := cleanupStaleSocket(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("stale socket remains: %v", err)
	}
}

func TestDoNotRemoveRegularFileOrSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.sock")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleSocket(path); err == nil {
		t.Fatal("must reject regular file")
	}
	if data, _ := os.ReadFile(path); string(data) != "keep" {
		t.Fatal("regular file changed")
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleSocket(link); err == nil {
		t.Fatal("must reject symlink")
	}
}
