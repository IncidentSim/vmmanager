package controlapi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationSecurity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	base := `{"state_dir":"/unused","allowed_user_ids":[11],"shared_image_ids":["ubuntu"],"credentials":[{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","user_id":11}]}`
	if e := os.WriteFile(path, []byte(base), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	if c.ListenAddr != "127.0.0.1:8090" || c.MaxVMs != 2 || c.MaxVMsPerUser != 1 {
		t.Fatal("missing defaults")
	}
	c.ListenAddr = "0.0.0.0:8090"
	if e = c.Validate(); e == nil {
		t.Fatal("public plaintext accepted")
	}
	c.TLSCertFile = "cert"
	c.TLSKeyFile = "key"
	if e = c.Validate(); e != nil {
		t.Fatal(e)
	}
	c.Terminal.PublicOrigin = "https://term.test"
	c.Terminal.ListenAddr = "0.0.0.0:8443"
	if e = c.Validate(); e == nil {
		t.Fatal("public plaintext terminal accepted")
	}
	c.Terminal.PublicOrigin = ""
	c.AllowedUserIDs = []int64{0}
	if e = c.Validate(); e == nil {
		t.Fatal("owner zero accepted")
	}
}
func TestPrivateCredentialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if e := os.WriteFile(path, []byte("one-secret\n"), 0600); e != nil {
		t.Fatal(e)
	}
	c := Credential{TokenFile: path, UserID: 11}
	if e := c.Resolve(); e != nil {
		t.Fatal(e)
	}
	if c.SHA256 != digest("one-secret") {
		t.Fatal("wrong credential digest")
	}
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	c.SHA256 = ""
	if e := c.Resolve(); e == nil {
		t.Fatal("public credential accepted")
	}
}
