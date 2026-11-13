package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/deployment"
	"github.com/petruekhin/vibe-firecracker/internal/controlapi"
	"github.com/petruekhin/vibe-firecracker/internal/localcfg"
	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

// The shipped configuration must be accepted by the real vmapi and backend
// validators and agree with the shipped pool.conf, image ID and prefix.
func TestShippedConfigIsValid(t *testing.T) {
	raw, err := deployment.Files.ReadFile("local/vmapi.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgText := strings.ReplaceAll(string(raw), "/var/lib/vmmanager/api", dir)
	for f, data := range map[string][]byte{"api-token": []byte("tok\n"), "ssh-master.key": make([]byte, 32), "known_hosts": []byte("x\n")} {
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "vmapi.json")
	if err := os.WriteFile(p, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := controlapi.LoadConfig(p)
	if err != nil {
		t.Fatalf("vmapi rejects the shipped config: %v", err)
	}
	if cfg.SharedImageIDs[0] != imageID || cfg.MaxVMs != 1 || cfg.Terminal.PublicOrigin != "" || cfg.ListenAddr != "127.0.0.1:8090" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	var top struct {
		Backend json.RawMessage `json:"backend"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	be, err := localcfg.Parse([]byte(strings.ReplaceAll(string(top.Backend), "/var/lib/vmmanager/api", dir)))
	if err != nil {
		t.Fatalf("backend rejects the shipped config: %v", err)
	}
	pc, _ := deployment.Files.ReadFile("local/pool.conf")
	pool, err := vmhelper.ParsePool(pc)
	if err != nil || pool.Prefix != prefix || pool.MaxSlots != be.MaxVMs || be.GuestPrefix != prefix {
		t.Fatalf("pool.conf disagrees with the backend config: %v %+v", err, pool)
	}
}

// The README must not point at paths or names that the shipped files lack.
func TestReadmeMatchesShippedFiles(t *testing.T) {
	b, _ := deployment.Files.ReadFile("local/README.txt")
	text := string(b)
	for _, want := range []string{"config/pool.conf", "config/vmapi.json config/known_hosts", "images/ubuntu.ext4", "assets/firecracker", "assets/vmlinux", "./bin/vmhost install"} {
		if !strings.Contains(text, want) {
			t.Fatalf("README lacks %q", want)
		}
	}
	if strings.Contains(strings.ToLower(text), "vibe") || strings.Contains(text, "/home/") || strings.Contains(text, "systemctl enable") {
		t.Fatal("README mentions an old name, a local path or an API service")
	}
}
