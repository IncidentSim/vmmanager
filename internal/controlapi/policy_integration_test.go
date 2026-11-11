package controlapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

type accessFixtureBackend struct {
	*vmlibtest.Backend
	keys *sshaccess.Store
}

func (b accessFixtureBackend) Prepare(ctx context.Context, r vmlib.PrepareRequest) (vmlib.HostVM, error) {
	if _, err := b.keys.Ensure(r.VMID, r.SSHPublicKey); err != nil {
		return vmlib.HostVM{}, err
	}
	return b.Backend.Prepare(ctx, r)
}

// Real SQLite owner execution plus encrypted credentials; the fake provisioning
// seam deliberately does not claim guest installation or live SSH evidence.
func TestIntegratedPolicyAccessAndSQLiteRestart(t *testing.T) {
	dir := t.TempDir()
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	keyfile := filepath.Join(dir, "master")
	if err := os.WriteFile(keyfile, master, 0600); err != nil {
		t.Fatal(err)
	}
	keys, err := sshaccess.OpenStore(filepath.Join(dir, "keys"), keyfile)
	if err != nil {
		t.Fatal(err)
	}
	b := vmlibtest.New(4, "ubuntu")
	b.SetCapabilities(vmlib.Capabilities{MaxVMs: 4, Profiles: []vmlib.Profile{{Name: "small", VCPU: 1, CPULimitMilli: 500, MemMiB: 500}}})
	start := func() (*httptest.Server, *vmbot.Controller, func()) {
		st, e := vmbot.OpenControlStore(filepath.Join(dir, "state"), 0)
		if e != nil {
			t.Fatal(e)
		}
		svc, e := vmlib.NewService(accessFixtureBackend{b, keys})
		if e != nil {
			t.Fatal(e)
		}
		ctl := vmbot.NewController(svc, st, &terms{}, vmbot.ControllerConfig{Profile: "small", DefaultTTL: 20 * time.Second, MaxTTL: 30 * time.Second})
		ctl.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11, 22}, SharedImages: []string{"ubuntu"}, MaxVMs: 4, MaxVMsPerUser: 2})
		if e = ctl.Start(context.Background()); e != nil {
			t.Fatal(e)
		}
		h, e := New(ctl, []Credential{{SHA256: digest("one"), UserID: 11}, {SHA256: digest("two"), UserID: 22}})
		if e != nil {
			t.Fatal(e)
		}
		if e = h.Configure(Options{Policy: Policy{DefaultProfile: "small", DefaultTTLSeconds: 20, MaxTTLSeconds: 30, TTL: true, GeneratedSSHKey: true, SuppliedSSHKey: true}, Keys: AccessKeys{ctl, keys}}); e != nil {
			t.Fatal(e)
		}
		srv := httptest.NewServer(h)
		return srv, ctl, func() { srv.Close(); ctl.Close(); st.Close() }
	}
	srv, ctl, closeFirst := start()
	closed := false
	defer func() {
		if !closed {
			closeFirst()
		}
	}()
	create := func(key, body string) (string, string) {
		r, p := rawCall(t, srv, "POST", "/v1/vms", "one", key, body)
		if r.StatusCode != 202 {
			t.Fatalf("create %d %s", r.StatusCode, p)
		}
		var v struct {
			VM string `json:"vm_id"`
			Op string `json:"operation_id"`
		}
		if err := json.Unmarshal(p, &v); err != nil {
			t.Fatal(err)
		}
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			o, _ := ctl.ControlOperation(11, v.Op)
			if o.Status == "succeeded" {
				return v.VM, v.Op
			}
			if o.Status == "failed" {
				t.Fatalf("create failed %+v", o)
			}
		}
		t.Fatal("create did not settle")
		return "", ""
	}
	generated, gop := create("generated", `{"image_id":"ubuntu","ttl_seconds":5}`)
	suppliedBody := `{"image_id":"ubuntu","ssh_public_key":"` + genKey(t) + `"}`
	supplied, _ := create("supplied", suppliedBody)
	r, p := rawCall(t, srv, "GET", "/v1/vms/"+generated+"/ssh-key", "one", "", "")
	if r.StatusCode != 200 || !strings.Contains(string(p), "PRIVATE KEY") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("key %d %s", r.StatusCode, p)
	}
	saved := string(p)
	for _, id := range []string{generated, supplied} {
		r, p = rawCall(t, srv, "GET", "/v1/vms/"+id+"/ssh-key", "two", "", "")
		if r.StatusCode != 404 {
			t.Fatalf("foreign %d %s", r.StatusCode, p)
		}
	}
	r, p = rawCall(t, srv, "GET", "/v1/vms/"+supplied+"/ssh-key", "one", "", "")
	if r.StatusCode != 409 || !strings.Contains(string(p), "key_not_generated") {
		t.Fatalf("supplied %d %s", r.StatusCode, p)
	}
	before, err := ctl.ControlVM(11, supplied)
	if err != nil || before.Resources.CPULimitMilli != 500 || before.TTLSeconds != 20 || before.ExpiresAt == nil {
		t.Fatalf("effective %+v %v", before, err)
	}
	closeFirst()
	closed = true
	// Alter the catalogue: admitted reservations and replay must not depend on it.
	b.SetCapabilities(vmlib.Capabilities{MaxVMs: 4, Profiles: []vmlib.Profile{{Name: "replacement", VCPU: 2, CPULimitMilli: 1000, MemMiB: 900}}})
	srv, ctl, closeSecond := start()
	defer closeSecond()
	if err := ctl.RecoverControl(); err != nil {
		t.Fatal(err)
	}
	after, err := ctl.ControlVM(11, supplied)
	if err != nil || *after.Resources != *before.Resources || !after.ExpiresAt.Equal(*before.ExpiresAt) {
		t.Fatalf("restart changed reservation %+v %v", after, err)
	}
	r, p = rawCall(t, srv, "POST", "/v1/vms", "one", "supplied", suppliedBody)
	if r.StatusCode != 202 {
		t.Fatalf("removed-profile replay %d %s", r.StatusCode, p)
	}
	r, p = rawCall(t, srv, "GET", "/v1/vms/"+generated+"/ssh-key", "one", "", "")
	if r.StatusCode != 200 || string(p) != saved {
		t.Fatalf("key stability %d %s", r.StatusCode, p)
	}
	// Real owner expiry across restart must remove only its VM.
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if _, e := ctl.ControlVM(11, generated); errors.Is(e, vmbot.ErrNoVM) {
			o, _ := ctl.ControlOperation(11, gop)
			if o.Status != "succeeded" {
				t.Fatal(o)
			}
			return
		}
	}
	t.Fatal("expired VM not deleted after SQLite recovery")
}
