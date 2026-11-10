package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// JWT mode: no api_token_file / service credential, synthetic usernames only.
func TestCheckConfigJWTSigningModeNeedsNoServiceCredential(t *testing.T) {
	d := t.TempDir()
	os.Chmod(d, 0700)
	token := filepath.Join(d, "tg")
	os.WriteFile(token, []byte("placeholder-tg"), 0600)
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	key := filepath.Join(d, "jwt-private.pem")
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600)
	cfg := filepath.Join(d, "config.json")
	raw := `{"version":1,"telegram_token_file":"` + token + `","api_url":"http://127.0.0.1:8090","allowed_usernames":["@Alice_Example"],"jwt":{"private_key_file":"` + key + `","issuer":"x-bot","audience":"x-api","ttl_seconds":28800},"delivery_dir":"` + filepath.Join(d, "delivery") + `","image_id":"ubuntu"}`
	os.WriteFile(cfg, []byte(raw), 0600)
	var logs strings.Builder
	logger := log.New(&logs, "", 0)
	if e := run(context.Background(), []string{"-config", cfg, "-check-config"}, logger); e != nil {
		t.Fatal(e)
	}
	// a broken signing key is caught by -check-config, without echoing key data
	os.WriteFile(key, []byte("not a key"), 0600)
	e := run(context.Background(), []string{"-config", cfg, "-check-config"}, logger)
	if e == nil || strings.Contains(e.Error(), "not a key") {
		t.Fatalf("invalid key must fail check without echo: %v", e)
	}
}

func TestHTTPFrontendExecutableConfigAndExplicitImport(t *testing.T) {
	d := t.TempDir()
	os.Chmod(d, 0700)
	token := filepath.Join(d, "tg")
	api := filepath.Join(d, "api")
	os.WriteFile(token, []byte("placeholder-tg"), 0600)
	os.WriteFile(api, []byte("placeholder-api"), 0600)
	cfg := filepath.Join(d, "config.json")
	delivery := filepath.Join(d, "delivery")
	raw := `{"version":1,"telegram_token_file":"` + token + `","api_token_file":"` + api + `","api_url":"http://127.0.0.1:8090","allowed_user_ids":[42],"delivery_dir":"` + delivery + `","image_id":"ubuntu"}`
	os.WriteFile(cfg, []byte(raw), 0600)
	logger := log.New(io.Discard, "", 0)
	if e := run(context.Background(), []string{"-config", cfg, "-check-config"}, logger); e != nil {
		t.Fatal(e)
	}
	old := filepath.Join(d, "legacy")
	os.Mkdir(old, 0700)
	source := filepath.Join(old, "state.json")
	os.WriteFile(source, []byte(`{"version":1,"offset":900,"generation":0}`), 0600)
	os.WriteFile(filepath.Join(old, "lock"), nil, 0600)
	if e := run(context.Background(), []string{"-config", cfg, "-import-legacy-offset", source}, logger); e == nil {
		t.Fatal("stopped acknowledgement missing")
	}
	if e := run(context.Background(), []string{"-config", cfg, "-import-legacy-offset", source, "-legacy-stopped"}, logger); e != nil {
		t.Fatal(e)
	}
	if e := run(context.Background(), []string{"-config", cfg, "-import-legacy-offset", source, "-legacy-stopped"}, logger); e == nil {
		t.Fatal("import replay")
	}
	if e := run(context.Background(), nil, logger); e == nil {
		t.Fatal("config missing")
	}
}
