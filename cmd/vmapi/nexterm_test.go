package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/nextermaccess"
	"github.com/petruekhin/vibe-firecracker/internal/nextermaccess/nxtest"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// Real config/startup/controller/SQLite/HTTP/SDK with only the VM and Nexterm
// servers faked. The executable trap proves terminal admission and replay never
// execute ttyd even when the legacy gateway remains configured for API routing.
func TestNextermExecutableNoTTYDAndStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	admin := filepath.Join(dir, "admin.json")
	if err := os.WriteFile(admin, []byte(`{"username":"root","password":"adminpw"}`), 0600); err != nil {
		t.Fatal(err)
	}
	nx := httptest.NewServer(nxtest.New())
	defer nx.Close()
	marker := filepath.Join(dir, "ttyd-started")
	trap := filepath.Join(dir, "ttyd")
	if err := os.WriteFile(trap, []byte("#!/bin/sh\nprintf invoked > '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	freeAddr := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()
		return addr
	}
	addr, gateway := freeAddr(), freeAddr()
	state := filepath.Join(dir, "nexterm-state.json")
	cfg := map[string]any{
		"listen_addr": addr, "state_dir": filepath.Join(dir, "state"),
		"allowed_user_ids": []int64{11, 22}, "shared_image_ids": []string{"ubuntu"}, "max_vms": 2, "max_vms_per_user": 1,
		"credentials": []any{map[string]any{"sha256": digestOf("owner"), "user_id": 11}, map[string]any{"sha256": digestOf("other"), "user_id": 22}},
		"backend":     testBackendPolicy,
		"terminal":    map[string]any{"listen_addr": gateway, "public_origin": "https://terminal.example", "ttyd_path": trap, "ssh_key_file": filepath.Join(dir, "unused-legacy-key")},
		"nexterm":     map[string]any{"enabled": true, "url": nx.URL, "admin_credentials_file": admin, "state_file": state},
	}
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "api.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	fb := &fakeOpener{b: vmlibtest.New(2, "ubuntu")}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	pr := startRun(path, fb.open)
	defer pr.kill()
	hc := &http.Client{Timeout: 3 * time.Second}
	base := "http://" + addr
	waitReady := func() {
		for {
			if r, err := hc.Get(base + "/healthz"); err == nil {
				r.Body.Close()
				if r.StatusCode == 200 {
					return
				}
			}
			select {
			case err := <-pr.done:
				pr.stopped = true
				t.Fatalf("API startup: %v", err)
			case <-ctx.Done():
				t.Fatal("API startup timeout")
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	waitReady()
	owner, err := controlclient.NewUser(base, "owner", 11, hc)
	if err != nil {
		t.Fatal(err)
	}
	other, err := controlclient.NewUser(base, "other", 22, hc)
	if err != nil {
		t.Fatal(err)
	}
	created, err := owner.Create(ctx, 11, "nx-create", controlclient.CreateRequest{ImageID: "ubuntu"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Wait(ctx, 11, created.OperationID, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// A create with no browser read must already have durable provisioning intent.
	pr.stop(t)
	tracked, err := nextermaccess.OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := tracked.Get(created.VMID); !ok || rec.Owner != 11 {
		t.Fatal("create did not track browser intent")
	}
	// Simulate a crash before the secondary store write: startup must recover
	// from the controller's authoritative durable VM snapshot.
	if err = tracked.Delete(created.VMID); err != nil {
		t.Fatal(err)
	}
	tracked.Close()
	pr = startRun(path, fb.open)
	defer pr.kill()
	waitReady()
	if _, err = other.Terminal(ctx, 22, created.VMID, "foreign"); !isAPI(err, 404) {
		t.Fatalf("foreign terminal: %v", err)
	}
	term, err := owner.Terminal(ctx, 11, created.VMID, "nx-terminal")
	if err != nil {
		t.Fatal(err)
	}
	op, err := owner.Wait(ctx, 11, term.OperationID, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		URL    string `json:"url"`
		Status string `json:"browser_status"`
	}
	if err = json.Unmarshal(op.Result, &result); err != nil || result.Status != "unsupported" || result.URL != "" {
		t.Fatalf("terminal result: %v status=%q", err, result.Status)
	}
	ssh, err := owner.SSH(ctx, 11, created.VMID)
	if err != nil || ssh.Host == "" || ssh.BrowserStatus != "unsupported" {
		t.Fatalf("raw SSH lost: %v status=%q", err, ssh.BrowserStatus)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("ttyd executed")
	}
	// Store lock remains API-owned until shutdown; there is no bot store opener.
	if s, err := nextermaccess.OpenStore(state); err == nil {
		s.Close()
		t.Fatal("API did not hold store lock")
	}
	pr.stop(t)
	s, err := nextermaccess.OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec, ok := s.Get(created.VMID)
	if !ok || rec.Owner != 11 || rec.Stage != nextermaccess.StageUnsupported {
		t.Fatal("API binding not durable")
	}
}
