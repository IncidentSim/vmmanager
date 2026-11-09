package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testID = "0123456789abcdef0123456789abcdef"
const replacementID = "abcdef0123456789abcdef0123456789"

func apiServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "fc-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return socket
}
func instanceInfo(w http.ResponseWriter, id, state string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":%q,"state":%q,"vmm_version":"1.0.0","app_name":"Firecracker"}`, id, state)
}
func writeReceipt(t *testing.T, socket, id, result, code, status string) {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"invocation_id": id, "result": result, "exit_code": code, "exit_status": status})
	if err := os.WriteFile(filepath.Join(filepath.Dir(socket), id+".exit.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestConnectRejectsOfflineBusyAndInvalidIDsWithoutDiskChanges(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		id, state string
		offline   bool
	}{
		{testID, "Not started", true}, {testID, "Running", false}, {"../escape", "Not started", false}, {"MAIN", "Not started", false},
	} {
		t.Run(tc.id+tc.state+fmt.Sprint(tc.offline), func(t *testing.T) {
			root := t.TempDir()
			disk := filepath.Join(root, "disk")
			os.WriteFile(disk, []byte("disk"), 0600)
			var mu sync.Mutex
			puts := 0
			socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					instanceInfo(w, tc.id, tc.state)
					return
				}
				mu.Lock()
				puts++
				mu.Unlock()
				w.WriteHeader(204)
			})
			if tc.offline {
				socket = filepath.Join(filepath.Dir(socket), "missing.sock")
			}
			start := time.Now()
			vm, err := startManagedVM(context.Background(), root, socket, disk)
			if err == nil || vm != nil {
				t.Fatalf("vm=%v err=%v", vm, err)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("not fail-fast")
			}
			info, _ := os.Stat(disk)
			if info.Mode().Perm() != 0600 {
				t.Fatal("changed disk before readiness")
			}
			mu.Lock()
			defer mu.Unlock()
			if puts != 0 {
				t.Fatal("configured unavailable server")
			}
		})
	}
}
func TestConnectionUsesSDKAndReceiptNotReplacement(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "disk")
	os.WriteFile(disk, []byte("disk"), 0600)
	socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			instanceInfo(w, testID, "Not started")
			return
		}
		w.WriteHeader(204)
	})
	vm, err := startManagedVM(context.Background(), root, socket, disk)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := vm.Wait(ctx); err == nil {
		t.Fatal("no receipt was accepted")
	}
	writeReceipt(t, socket, testID, "success", "exited", "0")
	if err := vm.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := vm.Stop(); err != nil {
		t.Fatal(err)
	}
}
func TestAbortUsesSDKActionAndWaitsForReceipt(t *testing.T) {
	var socket string
	actions := make(chan string, 1)
	socket = apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			instanceInfo(w, testID, "Running")
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		actions <- body["action_type"]
		writeReceipt(t, socket, testID, "success", "exited", "0")
		w.WriteHeader(204)
	})
	vm := &managedVM{socket: socket, id: testID, started: true}
	if err := vm.Stop(); err != nil {
		t.Fatal(err)
	}
	if got := <-actions; got != "SendCtrlAltDel" {
		t.Fatal(got)
	}
}
func TestAbortDoesNotRebootReplacementOrUnstartedVM(t *testing.T) {
	for _, started := range []bool{true, false} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("rebooted replacement")
				}
				instanceInfo(w, replacementID, "Not started")
			})
			vm := &managedVM{socket: socket, id: testID, started: started}
			if err := vm.Stop(); err == nil || !strings.Contains(err.Error(), "restart") {
				t.Fatalf("unsafe cleanup: %v", err)
			}
		})
	}
}
func TestAbortWaitsForReceiptAfterLostActionResponse(t *testing.T) {
	var socket string
	socket = apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			instanceInfo(w, testID, "Running")
			return
		}
		go func() { time.Sleep(20 * time.Millisecond); writeReceipt(t, socket, testID, "success", "exited", "0") }()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		fmt.Fprint(w, `{"fault_message":"response lost"}`)
	})
	vm := &managedVM{socket: socket, id: testID, started: true}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := vm.abort(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestAbortRequiresReceiptEvenWhenActionSucceeded(t *testing.T) {
	socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			instanceInfo(w, testID, "Running")
			return
		}
		w.WriteHeader(204)
	})
	vm := &managedVM{socket: socket, id: testID, started: true}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := vm.abort(ctx); err == nil {
		t.Fatal("action accepted without exit proof")
	}
}
func TestConfigureRechecksIdentityBeforeTouchingDisk(t *testing.T) {
	socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("configured replacement")
			w.WriteHeader(204)
			return
		}
		instanceInfo(w, replacementID, "Not started")
	})
	root := t.TempDir()
	disk := filepath.Join(root, "disk")
	os.WriteFile(disk, []byte("disk"), 0600)
	vm := &managedVM{socket: socket, id: testID}
	if err := vm.configure(context.Background(), root, disk); err == nil {
		t.Fatal("configured replacement")
	}
	info, _ := os.Stat(disk)
	if info.Mode().Perm() != 0600 {
		t.Fatal("disk changed before identity check")
	}
}
func TestConfigureDetectsReplacementDuringConfiguration(t *testing.T) {
	gets := 0
	socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			gets++
			if gets == 1 {
				instanceInfo(w, testID, "Not started")
			} else {
				instanceInfo(w, replacementID, "Not started")
			}
			return
		}
		if r.URL.Path == "/actions" {
			t.Error("started replacement")
		}
		w.WriteHeader(204)
	})
	root := t.TempDir()
	disk := filepath.Join(root, "disk")
	os.WriteFile(disk, []byte("disk"), 0600)
	vm := &managedVM{socket: socket, id: testID}
	if err := vm.configure(context.Background(), root, disk); err == nil {
		t.Fatal("replacement not detected")
	}
	writeReceipt(t, socket, testID, "success", "exited", "0")
	if err := vm.Stop(); err == nil {
		t.Fatal("old receipt allowed cleanup after configuring unknown process")
	}
}
func TestBuildPreflightHasNoImageSideEffects(t *testing.T) {
	for _, state := range []string{"offline", "Running"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			sources := t.TempDir()
			base := filepath.Join(sources, "base")
			key := filepath.Join(sources, "key")
			os.WriteFile(base, []byte("base unchanged"), 0600)
			os.WriteFile(key, []byte("key"), 0600)
			output := filepath.Join(sources, "absent", "output.ext4")
			socket := filepath.Join(t.TempDir(), "missing.sock")
			if state != "offline" {
				socket = apiServer(t, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						t.Error("configured busy server")
					}
					instanceInfo(w, testID, state)
				})
			}
			if err := buildInWorkspace(context.Background(), root, socket, base, key, output); err == nil {
				t.Fatal("build accepted unavailable VMM")
			}
			if _, err := os.Stat(filepath.Dir(output)); !os.IsNotExist(err) {
				t.Fatal("output directory created", err)
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 || entries[0].Name() != ".client.lock" {
				t.Fatalf("working image created: %v", entries)
			}
			data, _ := os.ReadFile(base)
			if string(data) != "base unchanged" {
				t.Fatal("base changed")
			}
		})
	}
}
func TestBuildRetainsDiskOnConfigurationFailure(t *testing.T) {
	root := t.TempDir()
	sources := t.TempDir()
	base := filepath.Join(sources, "base")
	key := filepath.Join(sources, "key")
	os.WriteFile(base, []byte("base unchanged"), 0600)
	os.WriteFile(key, []byte("key"), 0600)
	output := filepath.Join(sources, "output.ext4")
	socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			instanceInfo(w, testID, "Not started")
			return
		}
		if r.URL.Path == "/actions" {
			t.Error("started guest after configuration error")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprint(w, `{"fault_message":"config rejected"}`)
	})
	err := buildInWorkspace(context.Background(), root, socket, base, key, output)
	if err == nil || !strings.Contains(err.Error(), "disk retained at") || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("nonactionable failure: %v", err)
	}
	disks, _ := filepath.Glob(filepath.Join(root, ".postgres-build-*.ext4"))
	if len(disks) != 1 {
		t.Fatal("disk not retained", disks)
	}
	data, _ := os.ReadFile(disks[0])
	if string(data) != "base unchanged" {
		t.Fatal("working copy changed")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("published without clean receipt")
	}
}
func TestReadinessRequestHasTwoSecondDeadline(t *testing.T) {
	socket := apiServer(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	start := time.Now()
	vm, err := connectVM(context.Background(), socket)
	if vm != nil || err == nil {
		t.Fatal("stalled API accepted")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("readiness unbounded: %v", elapsed)
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
