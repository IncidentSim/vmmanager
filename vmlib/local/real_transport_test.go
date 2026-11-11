//go:build linux

package local_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

// Exercise the actual SDK over a Unix HTTP listener, including its response
// decoders. ConnState counts real accepted sockets, not client factory calls.
func transportVMM(t *testing.T, handler http.Handler) (local.SDKVMM, func() int) {
	t.Helper()
	// Keep the socket below Unix sockaddr_un's path limit even with a long TMPDIR.
	dir, err := os.MkdirTemp("", "fc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := local.SocketPath(dir, 0)
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	open := make(map[net.Conn]bool)
	server := &http.Server{Handler: handler, ConnState: func(c net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch state {
		case http.StateNew:
			open[c] = true
		case http.StateClosed, http.StateHijacked:
			delete(open, c)
		}
	}}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return local.SDKVMM{RunDir: dir}, func() int { mu.Lock(); defer mu.Unlock(); return len(open) }
}

func requireTransportDrained(t *testing.T, open func() int) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if open() == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("SDK left %d Unix HTTP connections open after completed operations", open())
}

func TestSDKVMMTransportDoesNotRetainConnections(t *testing.T) {
	for _, scenario := range []string{"status", "configure-start", "shutdown-retry", "error-body"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			seen := make(map[string]int)
			v, open := transportVMM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				seen[r.Method+" "+r.URL.Path]++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if scenario == "error-body" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"fault_message":"test rejection"}`)
				} else if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"id":"0123456789abcdef0123456789abcdef","state":"Running","vmm_version":"1.0.0","app_name":"Firecracker"}`)
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			spec := local.MachineSpec{VCPU: 1, MemMiB: 128, Kernel: "/kernel", DiskJailPath: "/rootfs", Tap: "tap0", MAC: "06:00:00:00:00:01"}
			check := func(err error) {
				t.Helper()
				if scenario == "error-body" {
					if err == nil {
						t.Fatal("expected decoded SDK error")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 12; i++ {
				switch scenario {
				case "status":
					info, err := v.Info(ctx, 0)
					check(err)
					if info.State != "Running" {
						t.Fatalf("info: %+v", info)
					}
				case "configure-start":
					check(v.Configure(ctx, 0, spec))
					check(v.InstanceStart(ctx, 0))
				case "shutdown-retry":
					_, err := v.Info(ctx, 0)
					check(err)
					check(v.CtrlAltDel(ctx, 0)) // guest ignores CAD: the next iteration retries
				case "error-body":
					_, err := v.Info(ctx, 0)
					check(err)
					check(v.Configure(ctx, 0, spec))
					check(v.InstanceStart(ctx, 0))
					check(v.CtrlAltDel(ctx, 0))
				}
			}
			requireTransportDrained(t, open)
			mu.Lock()
			defer mu.Unlock()
			if scenario == "configure-start" {
				for _, path := range []string{"PUT /machine-config", "PUT /boot-source", "PUT /drives/rootfs", "PUT /network-interfaces/eth0", "PUT /actions"} {
					if seen[path] != 12 {
						t.Errorf("%s: got %d requests, want 12", path, seen[path])
					}
				}
			}
		})
	}
}

func TestSDKVMMTransportPreservesCallerDeadline(t *testing.T) {
	for _, operation := range []string{"info", "configure", "start", "shutdown"} {
		t.Run(operation, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			arrived := make(chan struct{}, 1)
			v, _ := transportVMM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				arrived <- struct{}{}
				<-release
				w.WriteHeader(http.StatusNoContent)
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			start := time.Now()
			var err error
			switch operation {
			case "info":
				_, err = v.Info(ctx, 0)
			case "configure":
				err = v.Configure(ctx, 0, local.MachineSpec{VCPU: 1, MemMiB: 128})
			case "start":
				err = v.InstanceStart(ctx, 0)
			case "shutdown":
				err = v.CtrlAltDel(ctx, 0)
			}
			if err == nil || ctx.Err() != context.DeadlineExceeded {
				t.Fatalf("caller deadline not preserved: ctx=%v err=%v", ctx.Err(), err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("operation ignored caller deadline")
			}
			select {
			case <-arrived:
			default:
				t.Fatal("request never reached Unix HTTP listener")
			}
		})
	}
}

func TestSDKVMMTransportTypedActions(t *testing.T) {
	actions := make(chan string, 2)
	v, open := transportVMM(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var action struct {
			ActionType string `json:"action_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&action); err != nil {
			t.Error(err)
		}
		actions <- action.ActionType
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := v.InstanceStart(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := v.CtrlAltDel(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got := <-actions; got != "InstanceStart" {
		t.Fatal(got)
	}
	if got := <-actions; got != "SendCtrlAltDel" {
		t.Fatal(got)
	}
	requireTransportDrained(t, open)
}
