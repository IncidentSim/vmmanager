package controlapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigurationSecurity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	base := `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_user_ids":[11],"shared_image_ids":["ubuntu"],"credentials":[{"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","user_id":11}]}`
	if e := os.WriteFile(path, []byte(base), 0600); e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	if c.ListenAddr != "127.0.0.1:8090" || c.MaxVMs != 2 || c.MaxVMsPerUser != 2 {
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

// testBackend is a valid private local backend policy (the former vmd
// backend.local object). No path in it is opened by config loading.
const testBackend = `{"work_root":"/w","images_dir":"/i","run_dir":"/r","max_vms":2,"helper":"/usr/local/libexec/fc-vm-helper","ssh_user":"root","storage_mount":"/s","max_total_mem_mib":1024,"max_total_vcpu":2,"unit_mem_limit_mib":768,"guest_prefix":"10.213","pool_conf":"/p","profiles":[{"name":"small","vcpu":1,"mem_mib":256,"disk_mib":1024}]}`

func TestConfigBackend(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "c.json")
		if e := os.WriteFile(p, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		return p
	}
	cred := `"credentials":[{"sha256":"` + digest("x") + `","user_id":11}]`
	head := `{"state_dir":"/unused","allowed_user_ids":[11],` + cred
	if _, e := LoadConfig(write(head + `,"backend":` + testBackend + `}`)); e != nil {
		t.Fatal(e)
	}
	facade := `"facade":{"addr":"h:19450","server_name":"h","ca_file":"/a","cert_file":"/b","key_file":"/c"}`
	for name, body := range map[string]string{
		"missing backend":       head + `}`,
		"stale gRPC facade":     head + `,"backend":` + testBackend + `,` + facade + `}`,
		"facade only":           head + `,` + facade + `}`,
		"empty facade object":   head + `,"facade":{},"backend":` + testBackend + `}`,
		"former vmd shape":      head + `,"backend":{"kind":"local","local":` + testBackend + `}}`,
		"relative helper":       head + `,"backend":` + strings.Replace(testBackend, `"/usr/local/libexec/fc-vm-helper"`, `"helper"`, 1) + `}`,
		"api max above backend": head + `,"max_vms":3,"backend":` + testBackend + `}`,
		"unknown backend key":   head + `,"backend":` + strings.Replace(testBackend, `"max_vms":2`, `"max_vms":2,"trusted_owner_uid":0`, 1) + `}`,
	} {
		if _, e := LoadConfig(write(body)); e == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
}
