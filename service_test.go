package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartManagedUsesUnitAndOfficialInstanceInfo(t *testing.T) {
	for _, state := range []string{"Not started", "Running"} {
		t.Run(state, func(t *testing.T) {
			log := fakeSystemctl(t, "exit 0\n")
			root := t.TempDir()
			disk := filepath.Join(root, "disk")
			os.WriteFile(disk, []byte("disk"), 0600)
			socketDir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "fc-api-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(socketDir)
			socket := filepath.Join(socketDir, "api.sock")
			l, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			requests := make(chan string, 10)
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r.URL.Path
				if r.Method == "GET" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"id":"main","state":%q,"vmm_version":"1.0.0","app_name":"Firecracker"}`, state)
					return
				}
				w.WriteHeader(204)
			})}
			go srv.Serve(l)
			defer srv.Close()
			vm, err := startManagedVM(context.Background(), root, socket, disk)
			if vm == nil || (err != nil) != (state == "Running") {
				t.Fatalf("vm=%v err=%v", vm, err)
			}
			calls, _ := os.ReadFile(log)
			if string(calls) != "--no-ask-password start "+serviceUnit+"\n" {
				t.Fatal(string(calls))
			}
			if first := <-requests; first != "/" {
				t.Fatalf("not InstanceInfo GET /: %q", first)
			}
			if state == "Running" && len(requests) != 0 {
				t.Fatal("configured running instance")
			}
		})
	}
}

func TestPrepareDiskGrantsOnlyGroupAccess(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "run")
	os.Mkdir(dir, 0700)
	disk := filepath.Join(dir, "disk")
	os.WriteFile(disk, []byte("disk"), 0600)
	path, err := prepareDisk(root, disk)
	if err != nil || path != "/work/run/disk" {
		t.Fatalf("%s %v", path, err)
	}
	for name, want := range map[string]os.FileMode{dir: 0770, disk: 0660} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("permissions %s: %v %v", name, info, err)
		}
	}
}
func fakeSystemctl(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CALL_LOG\"\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("CALL_LOG", log)
	return log
}
func TestStopVerifiesState(t *testing.T) {
	log := fakeSystemctl(t, "case \"$2\" in stop) exit 0;; show) printf 'ActiveState=active\\n';; esac\n")
	if err := (&managedVM{}).Stop(); err == nil {
		t.Fatal("active VM allowed cleanup")
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "--no-ask-password stop "+serviceUnit) || !strings.Contains(string(calls), "--no-ask-password show") {
		t.Fatal(string(calls))
	}
}
func TestWaitReturnsFailureForKilledService(t *testing.T) {
	fakeSystemctl(t, "printf 'ActiveState=failed\\nSubState=failed\\nResult=signal\\nExecMainStatus=9\\nExecMainCode=2\\n'\n")
	if err := (&managedVM{}).Wait(context.Background()); err == nil {
		t.Fatal("killed service reported success")
	}
}
func TestWaitRespectsCancellation(t *testing.T) {
	fakeSystemctl(t, "printf 'ActiveState=active\\nSubState=running\\n'\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := (&managedVM{}).Wait(ctx); err == nil {
		t.Fatal("cancellation ignored")
	}
}
