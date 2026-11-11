package vmbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestTerminalRealStoreLeasesDedicatedKeyThroughFakeRunner(t *testing.T) {
	d := t.TempDir()
	master := filepath.Join(d, "master")
	if err := sshaccess.GenerateMasterKeyFile(master); err != nil {
		t.Fatal(err)
	}
	store, err := sshaccess.OpenStore(filepath.Join(d, "store"), master)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Ensure("real-store-vm", ""); err != nil {
		t.Fatal(err)
	}
	owner, err := store.OwnerPrivateKey("real-store-vm")
	if err != nil {
		t.Fatal(err)
	}
	e := newTerm(t, func(cfg *ManagerConfig) { cfg.Credentials = store; cfg.SSHKeyFile = "" })
	if err = e.m.Start(context.Background(), target("real-store-vm", tokA)); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(e.m.cfg.KeyRuntimeDir, "*.key"))
	if err != nil || len(paths) != 1 {
		t.Fatal("terminal lease missing", err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, owner) || !bytes.Contains(raw, []byte("OPENSSH PRIVATE KEY")) {
		t.Fatal("not the independent terminal identity")
	}
	fi, err := os.Stat(paths[0])
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Fatal("nonprivate lease", err)
	}
	e.m.Revoke("real-store-vm")
	if _, err = os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatal("real Store lease not cleaned after exit", err)
	}
	if _, err = store.OwnerPrivateKey("real-store-vm"); err != nil {
		t.Fatal("terminal revoke removed owner credentials", err)
	}
}

type leaseCredentials struct {
	mu        sync.Mutex
	paths     map[string]string
	released  map[string]int
	fail      bool
	onRelease func(string)
}

func (c *leaseCredentials) TerminalKeyFile(id, dir string) (string, func(), error) {
	if c.fail {
		return "", nil, errors.New("unavailable")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	p := filepath.Join(dir, id+".key")
	if err := os.WriteFile(p, []byte("dedicated terminal test identity "+id), 0600); err != nil {
		return "", nil, err
	}
	c.mu.Lock()
	c.paths[id] = p
	c.mu.Unlock()
	return p, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.onRelease != nil {
			c.onRelease(id)
		}
		c.released[id]++
		os.Remove(p)
	}, nil
}
func (c *leaseCredentials) path(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths[id]
}
func (c *leaseCredentials) releases(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.released[id]
}
func newLeaseCredentials() *leaseCredentials {
	return &leaseCredentials{paths: map[string]string{}, released: map[string]int{}}
}

func TestTerminalDedicatedLeaseLivesUntilProcessExit(t *testing.T) {
	c := newLeaseCredentials()
	e := newTerm(t, func(cfg *ManagerConfig) { cfg.Credentials = c; cfg.SSHKeyFile = "" })
	if err := e.m.Start(context.Background(), target("lease-a", tokA)); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Start(context.Background(), target("lease-b", tokB)); err != nil {
		t.Fatal(err)
	}
	a, b := c.path("lease-a"), c.path("lease-b")
	if a == b || a == "" || b == "" {
		t.Fatal("not per VM")
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatal("lease removed while process lives", err)
	}
	raw, err := os.ReadFile(e.m.sockPath("lease-a") + ".argv")
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err = json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	found := false
	for i, v := range args {
		if v == "-i" && i+1 < len(args) && args[i+1] == a {
			found = true
		}
	}
	if !found {
		t.Fatal("ttyd did not receive dedicated identity")
	}
	e.m.mu.Lock()
	s := e.m.sess["lease-a"]
	e.m.mu.Unlock()
	c.mu.Lock()
	c.onRelease = func(id string) {
		if id == "lease-a" {
			if err := s.cmd.Process.Signal(syscall.Signal(0)); err == nil {
				t.Error("credential released before subprocess Wait")
			}
		}
	}
	c.mu.Unlock()
	if c.releases("lease-a") != 0 {
		t.Fatal("early release")
	}
	e.m.Revoke("lease-a")
	if _, err = os.Stat(a); !os.IsNotExist(err) {
		t.Fatal("key remains after process exit", err)
	}
	if c.releases("lease-a") != 1 {
		t.Fatal("lease not released exactly once")
	}
	if _, err = os.Stat(b); err != nil {
		t.Fatal("other VM identity affected", err)
	}
	e.m.Close()
	if c.releases("lease-b") != 1 {
		t.Fatal("close did not release")
	}
}

func TestTerminalCredentialFailuresNeverUseLegacyFallback(t *testing.T) {
	c := newLeaseCredentials()
	c.fail = true
	e := newTerm(t, func(cfg *ManagerConfig) { cfg.Credentials = c })
	if err := e.m.Start(context.Background(), target("no-key", tokA)); err == nil {
		t.Fatal("missing dedicated key fell back to legacy")
	}
	if len(e.m.slots) != 0 {
		t.Fatal("failed start leaked slot")
	}
}
func TestTerminalFailedExecReleasesLease(t *testing.T) {
	c := newLeaseCredentials()
	e := newTerm(t, func(cfg *ManagerConfig) { cfg.Credentials = c; cfg.TTYDPath = "/nonexistent-ttyd" })
	if err := e.m.Start(context.Background(), target("failed-exec", tokA)); err == nil {
		t.Fatal("exec unexpectedly succeeded")
	}
	if c.releases("failed-exec") != 1 {
		t.Fatal("failed exec leaked lease")
	}
	if _, err := os.Stat(c.path("failed-exec")); !os.IsNotExist(err) {
		t.Fatal("key remains")
	}
}
