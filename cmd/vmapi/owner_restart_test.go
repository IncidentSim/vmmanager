package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// createCounter counts backend Prepare calls (the first step of a Create)
// reaching the shared fake.
type createCounter struct {
	vmlib.Backend
	creates atomic.Int32
}

func (c *createCounter) Prepare(ctx context.Context, r vmlib.PrepareRequest) (vmlib.HostVM, error) {
	c.creates.Add(1)
	return c.Backend.Prepare(ctx, r)
}

// A live VM's owner outlives an API restart on the same state: the VM is not
// re-created, stays active with its identity, and the restored owner accepts
// further commands through the public client. Real run(), real DBOS SQLite.
func TestOwnerSurvivesAPIRestart(t *testing.T) {
	dir := t.TempDir()
	cb := &createCounter{Backend: vmlibtest.New(3, "ubuntu")}
	fb := &fakeOpener{b: cb}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := l.Addr().String()
	l.Close()
	cfg := map[string]any{
		"listen_addr": addr, "state_dir": filepath.Join(dir, "state"),
		"allowed_user_ids": []int64{11}, "shared_image_ids": []string{"ubuntu"}, "max_vms": 2, "max_vms_per_user": 2,
		"credentials": []any{map[string]any{"sha256": digestOf("test-only-user-11"), "user_id": 11}},
		"backend":     testBackendPolicy,
	}
	b, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "api.json")
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hc := &http.Client{Timeout: 3 * time.Second}
	user, _ := controlclient.NewUser("http://"+addr, "test-only-user-11", 11, hc)
	healthy := func() {
		t.Helper()
		for until := time.Now().Add(10 * time.Second); time.Now().Before(until); time.Sleep(20 * time.Millisecond) {
			if p, e := hc.Get("http://" + addr + "/healthz"); e == nil {
				p.Body.Close()
				if p.StatusCode == 200 {
					return
				}
			}
		}
		t.Fatal("API not healthy")
	}
	wait := func(id string) {
		t.Helper()
		if o, e := user.Wait(ctx, 11, id, 20*time.Millisecond); e != nil || o.Status != "succeeded" {
			t.Fatalf("operation %s: %v %+v", id, e, o)
		}
	}
	pr := startRun(path, fb.open)
	defer pr.kill()
	healthy()
	created, e := user.Create(ctx, 11, "c1", controlclient.CreateRequest{Name: "keep", ImageID: "ubuntu"})
	if e != nil {
		t.Fatal(e)
	}
	wait(created.OperationID)
	pr.stop(t)

	pr2 := startRun(path, fb.open)
	defer pr2.kill()
	healthy()
	vm, e := user.VM(ctx, 11, created.VMID)
	if e != nil || vm.Phase != "active" || vm.Name != "keep" || vm.OwnerError {
		t.Fatalf("identity/phase after restart: %v %+v", e, vm)
	}
	stop, e := user.Stop(ctx, 11, created.VMID, "s1")
	if e != nil {
		t.Fatal(e)
	}
	wait(stop.OperationID)
	del, e := user.Delete(ctx, 11, created.VMID, "d1")
	if e != nil {
		t.Fatal(e)
	}
	wait(del.OperationID)
	if n := cb.creates.Load(); n != 1 {
		t.Fatalf("backend Prepare called %d times across restart", n)
	}
	pr2.stop(t)
}
