package tgbot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped frontend template must load with the real frontend loader (not
// the legacy embedded-controller loader) and contain only placeholders.
func TestExampleConfigLoadsWithFrontendLoader(t *testing.T) {
	raw, e := os.ReadFile("../../deployment/vmbot/vmbot.example.json")
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"BEGIN", ":AA", "api.telegram", "facade", "state_dir"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("template must not contain %q", bad)
		}
	}
	p := filepath.Join(privateDir(t), "vmbot.json")
	if e = os.WriteFile(p, raw, 0600); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(p)
	if e != nil {
		t.Fatalf("template must load: %v", e)
	}
	if c.Profile != "small" || c.Resources != nil || !strings.HasPrefix(c.APIURL, "http://127.0.0.1") {
		t.Fatalf("template defaults: %+v", c)
	}
}

func TestJWTExampleConfigLoadsWithFrontendLoader(t *testing.T) {
	raw, e := os.ReadFile("../../deployment/vmbot/vmbot.jwt.example.json")
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"BEGIN", ":AA", "api.telegram", "eyJ", "api_token_file"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("template must not contain %q", bad)
		}
	}
	dir := privateDir(t)
	priv, _ := writeKeys(t, dir)
	cfg := strings.ReplaceAll(string(raw), "/home/USER/.config/vmbot-http/jwt-private.pem", priv)
	cfg = strings.ReplaceAll(cfg, "/home/USER", dir)
	p := filepath.Join(dir, "vmbot.json")
	if e = os.WriteFile(p, []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(p)
	if e != nil {
		t.Fatalf("template must load: %v", e)
	}
	if c.JWT == nil || c.JWT.TTLSeconds != 28800 || c.JWT.Issuer != "incident-sim-bot" || c.JWT.Audience != "incident-sim-api" || len(c.AllowedUsernames) == 0 || c.APITokenFile != "" {
		t.Fatalf("%+v", c)
	}
	if !strings.HasPrefix(c.APIURL, "http://127.0.0.1:8090") {
		t.Fatal(c.APIURL)
	}
}

func TestFrontendUnitTemplateHardeningAndFlags(t *testing.T) {
	b, e := os.ReadFile("../../deployment/vmbot/vmbot.service")
	if e != nil {
		t.Fatal(e)
	}
	txt := string(b)
	for _, want := range []string{"Restart=on-failure", "UMask=0077", "NoNewPrivileges=yes", "MemoryMax=", "TasksMax=",
		"KillSignal=SIGTERM", "KillMode=control-group", "TimeoutStopSec=", "ExecStart=%h/.local/bin/vmbot-http -config %h/.config/vmbot-http/vmbot.json"} {
		if !strings.Contains(txt, want) {
			t.Errorf("unit missing %q", want)
		}
	}
	for _, bad := range []string{"ExecStop", "token", "Environment"} {
		if strings.Contains(txt, bad) {
			t.Errorf("unit must not contain %q", bad)
		}
	}
}
