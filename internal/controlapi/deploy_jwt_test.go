package controlapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The JWT template must load with the real API loader and verifier, contain
// only synthetic usernames and no key material.
func TestJWTExampleConfigLoadsWithAPILoader(t *testing.T) {
	raw, err := os.ReadFile("../../deployment/vmapi/vmapi.jwt.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"BEGIN", "sha256", "api.telegram", "eyJ"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("template must not contain %q", bad)
		}
	}
	dir := t.TempDir()
	_, pub, _ := newKeyFiles(t, dir)
	cfg := strings.ReplaceAll(string(raw), "/home/USER/.config/vmapi/jwt-public.pem", pub)
	cfg = strings.ReplaceAll(cfg, "/home/USER", dir)
	p := filepath.Join(dir, "vmapi.json")
	if err = os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("template must load: %v", err)
	}
	names, err := c.CanonicalUsernames()
	if err != nil || len(names) == 0 || !strings.HasPrefix(names[0], "example_user") {
		t.Fatalf("usernames %v %v", names, err)
	}
	if c.JWT == nil || c.JWT.Issuer != "incident-sim-bot" || c.JWT.Audience != "incident-sim-api" || len(c.Credentials) != 0 || !c.ExposesAPI() {
		t.Fatalf("%+v", c)
	}
	if _, err = c.Verifier(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.ListenAddr, "127.0.0.1:") {
		t.Fatal("API root listener must stay loopback")
	}
}
