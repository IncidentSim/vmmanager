package vmbot

import (
	"os"
	"strings"
	"testing"
)

func TestExampleConfigIsValidAndHasOnlyPlaceholders(t *testing.T) {
	c, err := LoadConfig("../../deployment/vmbot/vmbot.example.json")
	if err != nil {
		t.Fatalf("example config must load: %v", err)
	}
	if c.Terminal.SSHUseBastion || c.Terminal.InsecureHTTPForTesting || !strings.HasPrefix(c.Terminal.PublicOrigin, "https://") {
		t.Fatalf("example must default to direct SSH and HTTPS: %+v", c.Terminal)
	}
	b, _ := os.ReadFile("../../deployment/vmbot/vmbot.example.json")
	for _, bad := range []string{"BEGIN", ":AA", "api.telegram"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("example must not contain %q", bad)
		}
	}
}

func TestUnitFileHardeningDirectives(t *testing.T) {
	b, err := os.ReadFile("../../deployment/vmbot/vmbot.service")
	if err != nil {
		t.Fatal(err)
	}
	txt := string(b)
	for _, want := range []string{"Restart=on-failure", "UMask=0077", "NoNewPrivileges=yes", "MemoryMax=", "TasksMax=",
		"KillSignal=SIGTERM", "KillMode=control-group", "TimeoutStopSec=", "ExecStart=%h/.local/bin/vmbot -config"} {
		if !strings.Contains(txt, want) {
			t.Errorf("unit missing %q", want)
		}
	}
	if strings.Contains(txt, "ExecStop") || strings.Contains(txt, "token") {
		t.Error("unit must not delete VMs on stop or carry a token")
	}
}
