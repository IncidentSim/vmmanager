package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestJailDiskPath(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "disk.ext4")
	os.WriteFile(disk, []byte("disk"), 0600)
	got, err := jailDiskPath(root, disk)
	if err != nil || got != "/work/disk.ext4" {
		t.Fatalf("%q %v", got, err)
	}
	os.Symlink(disk, filepath.Join(root, "alias"))
	os.Mkdir(filepath.Join(root, "nested"), 0700)
	os.Symlink(root, filepath.Join(root, "link"))
	for _, path := range []string{filepath.Join(root, "alias"), filepath.Join(root, "link", "disk.ext4"), filepath.Dir(root), root + "/nested/../disk.ext4"} {
		if _, err := jailDiskPath(root, path); err == nil {
			t.Errorf("accepted unsafe path %s", path)
		}
	}
}
func TestClientLockIsExclusiveAndStable(t *testing.T) {
	root := t.TempDir()
	first, err := lockWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if next, err := lockWorkspace(root); err == nil {
		next.Close()
		t.Fatal("concurrent client accepted")
	}
	first.Close()
	next, err := lockWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	if _, err := os.Stat(filepath.Join(root, ".client.lock")); err != nil {
		t.Fatal("lock must remain stable", err)
	}
}
func TestConfigureVMUsesOnlyJailPaths(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method == "GET" {
			instanceInfo(w, testID, "Not started")
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/boot-source" && body["kernel_image_path"] != "/kernel/vmlinux" {
			t.Errorf("kernel %v", body)
		}
		if r.URL.Path == "/drives/rootfs" && body["path_on_host"] != "/work/disk.ext4" {
			t.Errorf("disk %v", body)
		}
		if r.URL.Path == "/actions" && body["action_type"] != "InstanceStart" {
			t.Errorf("action %v", body)
		}
		w.WriteHeader(204)
	})}
	go srv.Serve(l)
	defer srv.Close()
	root := t.TempDir()
	disk := filepath.Join(root, "disk.ext4")
	os.WriteFile(disk, []byte("disk"), 0600)
	if _, err := startManagedVM(context.Background(), root, socket, disk); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/", "/", "/machine-config", "/boot-source", "/drives/rootfs", "/network-interfaces/eth0", "/", "/actions"}) {
		t.Fatal(paths)
	}
}
func TestPublishPreservesExistingOutput(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	out := filepath.Join(root, "out")
	os.WriteFile(src, []byte("new"), 0600)
	os.WriteFile(out, []byte("old"), 0600)
	if err := publishImage(src, out); err == nil {
		t.Fatal("overwrote existing image")
	}
	data, _ := os.ReadFile(out)
	if string(data) != "old" {
		t.Fatal(string(data))
	}
	os.Remove(out)
	if err := publishImage(src, out); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(out)
	if strings.TrimSpace(string(data)) != "new" {
		t.Fatal(string(data))
	}
}
