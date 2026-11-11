package controlapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Only metadata and live state are required: readiness must not depend on a
// polling WaitSSHReady implementation or a running durable executor.
type sshLifecycle struct {
	vmlib.Lifecycle
	info     vmlib.SSHInfo
	state    vmlib.VMState
	calls    atomic.Int32
	get      func(context.Context) (vmlib.VM, error)
	metadata func(context.Context) (vmlib.SSHInfo, error)
}

func (l *sshLifecycle) SSHInfo(ctx context.Context, _ string) (vmlib.SSHInfo, error) {
	l.calls.Add(1)
	if l.metadata != nil {
		return l.metadata(ctx)
	}
	return l.info, nil
}
func (l *sshLifecycle) Get(ctx context.Context, _ string) (vmlib.VM, error) {
	l.calls.Add(1)
	if l.get != nil {
		return l.get(ctx)
	}
	return vmlib.VM{ID: "ssh-vm", State: l.state}, nil
}

func sshServer(t *testing.T, lc *sshLifecycle, phase string) (*httptest.Server, *vmbot.Store) {
	t.Helper()
	st, err := vmbot.OpenControlStore(t.TempDir()+"/state", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Update(func(s *vmbot.State) {
		s.Generation = 1
		s.VMs = []vmbot.VMRecord{{ID: "ssh-vm", Token: strings.Repeat("a", 43), Generation: 1, Phase: phase, OwnerUserID: 11}}
	}); err != nil {
		t.Fatal(err)
	}
	c := vmbot.NewController(lc, st, &terms{}, vmbot.ControllerConfig{QueryTimeout: 80 * time.Millisecond})
	c.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11, 22}, MaxVMs: 2, MaxVMsPerUser: 1})
	h, err := New(c, []Credential{{SHA256: digest("one"), UserID: 11}, {SHA256: digest("two"), UserID: 22}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); st.Close() })
	return srv, st
}

func TestSSHReadinessHTTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closedPortText, _ := net.SplitHostPort(closed.Addr().String())
	closedPort, _ := strconv.Atoi(closedPortText)
	closed.Close()
	for _, tc := range []struct {
		name, phase string
		state       vmlib.VMState
		host        string
		port        int
		ready       bool
	}{
		{"reachable", vmbot.PhaseActive, vmlib.StateRunning, host, port, true},
		{"unreachable", vmbot.PhaseActive, vmlib.StateRunning, host, closedPort, false},
		{"absent host", vmbot.PhaseActive, vmlib.StateRunning, "", port, false},
		{"absent port", vmbot.PhaseActive, vmlib.StateRunning, host, 0, false},
		{"stopped backend", vmbot.PhaseActive, vmlib.StateStopped, host, port, false},
		{"stopped intent", "stopped", vmlib.StateRunning, host, port, false},
		{"deleting", vmbot.PhaseDeleting, vmlib.StateRunning, host, port, false},
		{"stopping", "stopping", vmlib.StateRunning, host, port, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An upstream Ready bit is deliberately stale; the HTTP result must be a
			// fresh observation, including when the VM is stopped or revoked.
			lc := &sshLifecycle{info: vmlib.SSHInfo{Host: tc.host, Port: tc.port, User: "root", Ready: true}, state: tc.state}
			if tc.ready {
				lc.info.Ready = false
			}
			srv, _ := sshServer(t, lc, tc.phase)
			code, body := call(t, srv, "GET", "/v1/vms/ssh-vm/ssh", "one", "", "", "")
			if code != 200 || body["ready"] != tc.ready || body["host"] != tc.host || body["user"] != "root" {
				t.Fatalf("HTTP %d %v; want ready=%v and preserved metadata", code, body, tc.ready)
			}
			if tc.phase != vmbot.PhaseActive && lc.calls.Load() != 1 {
				t.Fatal("inactive VM must return metadata without querying/probing readiness")
			}
		})
	}
}

func TestSSHReadinessOwnershipBeforeEffects(t *testing.T) {
	lc := &sshLifecycle{}
	srv, _ := sshServer(t, lc, vmbot.PhaseActive)
	for _, path := range []string{"/v1/vms/ssh-vm/ssh", "/v1/vms/missing/ssh"} {
		code, _ := call(t, srv, "GET", path, "two", "", "", "")
		if code != 404 {
			t.Fatalf("HTTP %d", code)
		}
	}
	if n := lc.calls.Load(); n != 0 {
		t.Fatalf("foreign/absent lookup made %d lifecycle calls", n)
	}
}

func TestSSHReadinessBoundedStateQuery(t *testing.T) {
	lc := &sshLifecycle{info: vmlib.SSHInfo{Host: "127.0.0.1", Port: 22, User: "root", Ready: true}}
	lc.get = func(ctx context.Context) (vmlib.VM, error) { <-ctx.Done(); return vmlib.VM{}, ctx.Err() }
	srv, _ := sshServer(t, lc, vmbot.PhaseActive)
	started := time.Now()
	code, body := call(t, srv, "GET", "/v1/vms/ssh-vm/ssh", "one", "", "", "")
	if code != 200 || body["ready"] != false || body["host"] != "127.0.0.1" {
		t.Fatalf("HTTP %d %v", code, body)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("handler waited %v", elapsed)
	}
}

func TestSSHReadinessMissingBackendState(t *testing.T) {
	lc := &sshLifecycle{info: vmlib.SSHInfo{Host: "127.0.0.1", Port: 22, User: "root", Ready: true}}
	lc.get = func(context.Context) (vmlib.VM, error) { return vmlib.VM{}, vmlib.ErrNotFound }
	srv, _ := sshServer(t, lc, vmbot.PhaseActive)
	code, body := call(t, srv, http.MethodGet, "/v1/vms/ssh-vm/ssh", "one", "", "", "")
	if code != 200 || body["ready"] != false || body["user"] != "root" {
		t.Fatalf("HTTP %d %v", code, body)
	}
}

func TestSSHReadinessMetadataTimeout(t *testing.T) {
	lc := &sshLifecycle{}
	lc.metadata = func(ctx context.Context) (vmlib.SSHInfo, error) { <-ctx.Done(); return vmlib.SSHInfo{}, ctx.Err() }
	srv, _ := sshServer(t, lc, vmbot.PhaseActive)
	started := time.Now()
	code, _ := call(t, srv, http.MethodGet, "/v1/vms/ssh-vm/ssh", "one", "", "", "")
	if code != 503 || time.Since(started) > time.Second {
		t.Fatalf("metadata query: HTTP %d after %v", code, time.Since(started))
	}
}

func TestSSHReadinessRechecksAfterSuccessfulTCP(t *testing.T) {
	for _, phase := range []string{"stopping", vmbot.PhaseDeleting, "backend stopped", "state failure"} {
		t.Run(phase, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			host, p, _ := net.SplitHostPort(listener.Addr().String())
			port, _ := strconv.Atoi(p)
			lc := &sshLifecycle{info: vmlib.SSHInfo{Host: host, Port: port, User: "root"}}
			srv, st := sshServer(t, lc, vmbot.PhaseActive)
			var gets atomic.Int32
			lc.get = func(context.Context) (vmlib.VM, error) {
				if gets.Add(1) == 2 {
					switch phase {
					case "backend stopped":
						return vmlib.VM{State: vmlib.StateStopped}, nil
					case "state failure":
						return vmlib.VM{}, errors.New("unavailable")
					default:
						if err := st.Update(func(s *vmbot.State) { s.VMs[0].Phase = phase }); err != nil {
							return vmlib.VM{}, err
						}
					}
				}
				return vmlib.VM{State: vmlib.StateRunning}, nil
			}
			code, body := call(t, srv, http.MethodGet, "/v1/vms/ssh-vm/ssh", "one", "", "", "")
			if code != 200 || body["ready"] != false || gets.Load() != 2 {
				t.Fatalf("HTTP %d %v, state checks=%d", code, body, gets.Load())
			}
		})
	}
}

func TestSSHReadinessConcurrentDeleteFence(t *testing.T) {
	lc := &sshLifecycle{info: vmlib.SSHInfo{Host: "127.0.0.1", Port: 22, User: "root", Ready: true}, state: vmlib.StateRunning}
	srv, st := sshServer(t, lc, vmbot.PhaseActive)
	lc.metadata = func(context.Context) (vmlib.SSHInfo, error) {
		err := st.Update(func(s *vmbot.State) { s.VMs[0].Phase = vmbot.PhaseDeleting })
		return lc.info, err
	}
	code, body := call(t, srv, http.MethodGet, "/v1/vms/ssh-vm/ssh", "one", "", "", "")
	if code != 200 || body["ready"] != false {
		t.Fatalf("HTTP %d %v", code, body)
	}
}
