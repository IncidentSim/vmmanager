package controlapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The API template must be accepted by the real API loader once its placeholder
// credential files exist as private files, and must carry no real secrets.
func TestExampleConfigLoadsWithAPILoader(t *testing.T) {
	raw, e := os.ReadFile("../../deployment/vmapi/vmapi.example.json")
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"BEGIN", ":AA", "sha256", "api.telegram"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("template must not contain %q", bad)
		}
	}
	dir := t.TempDir()
	cfg := strings.ReplaceAll(string(raw), "/home/USER", dir)
	for _, name := range []string{"service-token", "user-123456789-token"} {
		p := filepath.Join(dir, ".config", "vmapi", name)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(p, []byte("placeholder-"+name+"\n"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	cfgPath := filepath.Join(dir, "vmapi.json")
	if e = os.WriteFile(cfgPath, []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(cfgPath)
	if e != nil {
		t.Fatalf("template must load: %v", e)
	}
	if c.MaxVMs != 2 || c.MaxVMsPerUser != 1 || c.Terminal.SSHUseBastion || !strings.HasPrefix(c.Terminal.PublicOrigin, "https://") {
		t.Fatalf("template defaults: %+v", c)
	}
	if !strings.HasPrefix(c.ListenAddr, "127.0.0.1:") {
		t.Fatal("API template must bind loopback")
	}
}

func TestAPIUnitTemplate(t *testing.T) {
	b, e := os.ReadFile("../../deployment/vmapi/vmapi.service")
	if e != nil {
		t.Fatal(e)
	}
	txt := string(b)
	for _, want := range []string{"Restart=on-failure", "UMask=0077", "NoNewPrivileges=yes", "MemoryMax=", "TasksMax=",
		"KillSignal=SIGTERM", "KillMode=control-group", "TimeoutStopSec=", "ExecStart=@BINDIR@/vmapi -config %h/.config/vmapi/vmapi.json"} {
		if !strings.Contains(txt, want) {
			t.Errorf("unit missing %q", want)
		}
	}
	for _, bad := range []string{"ExecStop", "Environment", "EnvironmentFile"} {
		if strings.Contains(txt, bad) {
			t.Errorf("unit must not contain %q", bad)
		}
	}
}
