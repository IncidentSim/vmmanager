package tgbot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func privateDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if e := os.Chmod(d, 0700); e != nil {
		t.Fatal(e)
	}
	return d
}
func TestPrivateOffsetRestartLockAndMigration(t *testing.T) {
	d := privateDir(t)
	legacy := privateDir(t)
	source := filepath.Join(legacy, "state.json")
	raw := `{"version":2,"offset":700,"generation":9,"vms":[]}`
	os.WriteFile(source, []byte(raw), 0600)
	os.WriteFile(filepath.Join(legacy, "lock"), nil, 0600)
	s, e := OpenDelivery(d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = OpenDelivery(d); e == nil {
		t.Fatal("second poller acquired lock")
	}
	if e = s.ImportLegacy(source, false); e == nil {
		t.Fatal("missing stopped confirmation accepted")
	}
	if e = s.ImportLegacy(source, true); e != nil {
		t.Fatal(e)
	}
	if s.Offset() != 700 {
		t.Fatal(s.Offset())
	}
	if e = s.ImportLegacy(source, true); e == nil {
		t.Fatal("reimport allowed")
	}
	if e = s.Advance(701); e != nil {
		t.Fatal(e)
	}
	if e = s.Advance(1); e == nil {
		t.Fatal("offset regression")
	}
	s.Close()
	b, _ := os.ReadFile(source)
	if string(b) != raw {
		t.Fatal("legacy state changed")
	}
	s, e = OpenDelivery(d)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.Offset() != 701 {
		t.Fatal(s.Offset())
	}
	info, _ := os.Stat(filepath.Join(d, "delivery.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}
func TestOffsetRejectUnsafeSources(t *testing.T) {
	for _, raw := range []string{`{"version":3,"offset":9}`, `{"version":1,"offset":-1}`, `{"version":2}`, `{"version":1,"offset":null}`, `{"version":2,"offset":900,"offset":0}`, `{"version":2,"offset":1} {}`} {
		t.Run(raw, func(t *testing.T) {
			d := privateDir(t)
			s, e := OpenDelivery(d)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			old := privateDir(t)
			p := filepath.Join(old, "state.json")
			os.WriteFile(p, []byte(raw), 0600)
			os.WriteFile(filepath.Join(old, "lock"), nil, 0600)
			if s.ImportLegacy(p, true) == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	d := privateDir(t)
	os.WriteFile(filepath.Join(d, "state.json"), []byte(`{}`), 0600)
	if _, e := OpenDelivery(d); e == nil {
		t.Fatal("API/legacy state directory accepted")
	}
}
func TestConfigStrictAndPrivateSecrets(t *testing.T) {
	d := privateDir(t)
	p := filepath.Join(d, "config.json")
	token := filepath.Join(d, "telegram-token")
	cred := filepath.Join(d, "api-token")
	os.WriteFile(token, []byte("telegram-secret\n"), 0600)
	os.WriteFile(cred, []byte("service-secret\n"), 0600)
	good := `{"version":1,"telegram_token_file":"` + token + `","api_url":"http://127.0.0.1:8090","api_token_file":"` + cred + `","allowed_user_ids":[42],"delivery_dir":"` + filepath.Join(d, "delivery") + `","image_id":"ubuntu"}`
	os.WriteFile(p, []byte(good), 0600)
	cfg, e := LoadConfig(p)
	if e != nil {
		t.Fatal(e)
	}
	tg, api, e := cfg.Secrets()
	if e != nil || tg != "telegram-secret" || api != "service-secret" {
		t.Fatalf("secret reading failed: %v", e)
	}
	for _, bad := range []string{strings.Replace(good, `[42]`, `[0]`, 1), strings.Replace(good, `[42]`, `[]`, 1), strings.Replace(good, `[42]`, `[42,42]`, 1), strings.Replace(good, `"version":1`, `"version":2`, 1), strings.Replace(good, `"version":1`, `"state_dir":"old","version":1`, 1), good + ` {}`, `{"allowed_user_id":42,"state_dir":"old"}`} {
		os.WriteFile(p, []byte(bad), 0600)
		if _, e = LoadConfig(p); e == nil {
			t.Fatalf("accepted unsafe config: %s", bad)
		}
	}
	os.Chmod(token, 0644)
	if _, _, e = cfg.Secrets(); e == nil || strings.Contains(e.Error(), "telegram-secret") {
		t.Fatal("public token accepted / unsafe error")
	}
}
