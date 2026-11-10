package vmbot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseConfig(d string) Config {
	return Config{
		TelegramTokenFile: filepath.Join(d, "token"),
		AllowedUserID:     42,
		Facade:            FacadeConfig{Addr: "vmd.example.test:19450", ServerName: "vmd.example.test", CAFile: "/p/ca", CertFile: "/p/c", KeyFile: "/p/k"},
		VM:                VMConfig{ImageID: "base", Profile: "small"},
		StateDir:          filepath.Join(d, "state"),
		Terminal: TerminalConfig{
			ListenAddr: "127.0.0.1:8443", PublicOrigin: "https://bot.example.test:8443",
			TLSCertFile: "/c/cert.pem", TLSKeyFile: "/c/key.pem", SSHKeyFile: "/k/id",
		},
	}
}

func writeCfg(t *testing.T, mutate func(c *Config)) (string, string) {
	t.Helper()
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "token"), []byte("123:secret\n"), 0o600)
	c := baseConfig(d)
	if mutate != nil {
		mutate(&c)
	}
	b, _ := json.Marshal(c)
	p := filepath.Join(d, "vmbot.json")
	os.WriteFile(p, b, 0o600)
	return p, d
}

func TestLoadConfigValid(t *testing.T) {
	p, _ := writeCfg(t, nil)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowedUserID != 42 || c.Terminal.MaxClients != 2 || c.Terminal.TTYDPath != "ttyd" || c.Terminal.SSHPath != "ssh" {
		t.Fatalf("defaults: %+v", c)
	}
	tok, err := c.ReadToken()
	if err != nil || tok != "123:secret" {
		t.Fatalf("token %q %v", tok, err)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := map[string]func(c *Config){
		"http origin w/o test flag": func(c *Config) { c.Terminal.PublicOrigin = "http://bot.example.test" },
		"origin with path":          func(c *Config) { c.Terminal.PublicOrigin = "https://bot.example.test/x" },
		"origin with query":         func(c *Config) { c.Terminal.PublicOrigin = "https://bot.example.test?x=1" },
		"relative ssh key":          func(c *Config) { c.Terminal.SSHKeyFile = "id" },
		"zero user":                 func(c *Config) { c.AllowedUserID = 0 },
		"bad image id":              func(c *Config) { c.VM.ImageID = "Bad Image" },
		"public listen w/o tls": func(c *Config) {
			c.Terminal.TLSCertFile, c.Terminal.TLSKeyFile, c.Terminal.ListenAddr = "", "", "0.0.0.0:8443"
		},
		"cert without key": func(c *Config) { c.Terminal.TLSKeyFile = "" },
		"http on public listen": func(c *Config) {
			c.Terminal.PublicOrigin, c.Terminal.InsecureHTTPForTesting = "http://x.test", true
			c.Terminal.ListenAddr, c.Terminal.TLSCertFile, c.Terminal.TLSKeyFile = "0.0.0.0:80", "", ""
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := writeCfg(t, mut)
			if _, err := LoadConfig(p); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	// loopback + https origin + no TLS files = behind an existing HTTPS proxy: allowed.
	p, _ := writeCfg(t, func(c *Config) { c.Terminal.TLSCertFile, c.Terminal.TLSKeyFile = "", "" })
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("proxy mode: %v", err)
	}
	// http with explicit test flag on loopback is allowed.
	p, _ = writeCfg(t, func(c *Config) {
		c.Terminal.PublicOrigin, c.Terminal.InsecureHTTPForTesting = "http://127.0.0.1:8443", true
		c.Terminal.TLSCertFile, c.Terminal.TLSKeyFile = "", ""
	})
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("test http: %v", err)
	}
}

func TestLoadConfigUnknownFieldAndTokenPerms(t *testing.T) {
	p, _ := writeCfg(t, nil)
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `"allowed_user_id"`, `"bogus":1,"allowed_user_id"`, 1)), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("unknown field must fail")
	}
	p, d := writeCfg(t, nil)
	os.Chmod(filepath.Join(d, "token"), 0o644)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadToken(); err == nil {
		t.Fatal("group/world readable token file must be refused")
	}
}
