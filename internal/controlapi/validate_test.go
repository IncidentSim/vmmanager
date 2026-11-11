package controlapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func genKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func testPolicy() Policy {
	return Policy{DefaultProfile: "small", DefaultTTLSeconds: 3600, MaxTTLSeconds: 86400, TTL: true, SuppliedSSHKey: true}
}

func fieldMap(fe []FieldError) map[string]string {
	m := map[string]string{}
	for _, f := range fe {
		m[f.Field] = f.Code
	}
	return m
}

func TestParseCreateDefaultsAndNormalization(t *testing.T) {
	p := testPolicy()
	got, fe := ParseCreate([]byte(`{"image_id":"ubuntu"}`), p)
	if len(fe) != 0 || got.ImageID != "ubuntu" || got.Profile != "small" || got.TTLSeconds != 0 {
		t.Fatalf("%+v %v", got, fe)
	}
	if got.SSHPublicKey != "" {
		t.Fatal("no fake default public key")
	}
	// TTL feature off: the default is not materialised (nothing enforces it).
	p.TTL = false
	got, fe = ParseCreate([]byte(`{"image_id":"ubuntu"}`), p)
	if len(fe) != 0 || got.TTLSeconds != 0 {
		t.Fatalf("%+v %v", got, fe)
	}
}

func TestParseCreateFieldErrors(t *testing.T) {
	p := testPolicy()
	k := genKey(t)
	cases := []struct {
		name, body string
		want       map[string]string
	}{
		{"null fields", `{"image_id":null,"profile":null,"ttl_seconds":null}`, map[string]string{"image_id": "invalid_type", "profile": "invalid_type", "ttl_seconds": "invalid_type"}},
		{"empty object", `{}`, map[string]string{"image_id": "required"}},
		{"unknown", `{"image_id":"ubuntu","bogus":1,"owner_user_id":2}`, map[string]string{"bogus": "unknown_field", "owner_user_id": "unknown_field"}},
		{"resources removed", `{"image_id":"ubuntu","resources":{"vcpu":1,"memory_mib":128}}`, map[string]string{"resources": "unknown_field"}},
		{"types", `{"image_id":5,"name":[],"profile":true,"ttl_seconds":"9","ssh_public_key":1}`, map[string]string{"image_id": "invalid_type", "name": "invalid_type", "profile": "invalid_type", "ttl_seconds": "invalid_type", "ssh_public_key": "invalid_type"}},
		{"ttl fraction", `{"image_id":"ubuntu","ttl_seconds":1.5}`, map[string]string{"ttl_seconds": "invalid_type"}},
		{"ttl zero", `{"image_id":"ubuntu","ttl_seconds":0}`, map[string]string{"ttl_seconds": "out_of_range"}},
		{"ttl neg", `{"image_id":"ubuntu","ttl_seconds":-5}`, map[string]string{"ttl_seconds": "out_of_range"}},
		{"ttl max", `{"image_id":"ubuntu","ttl_seconds":86401}`, map[string]string{"ttl_seconds": "out_of_range"}},
		{"ttl huge", `{"image_id":"ubuntu","ttl_seconds":99999999999999999999}`, map[string]string{"ttl_seconds": "invalid_type"}},
		{"null body", `null`, map[string]string{"body": "invalid_type"}},
		{"array body", `[]`, map[string]string{"body": "invalid_type"}},
		{"bad json", `{"image_id":`, map[string]string{"body": "invalid_json"}},
		{"trailing", `{"image_id":"ubuntu"} {}`, map[string]string{"body": "invalid_json"}},
		{"name", `{"image_id":"ubuntu","name":"Bad Name"}`, map[string]string{"name": "invalid_format"}},
		{"image shape", `{"image_id":"../x"}`, map[string]string{"image_id": "invalid_format"}},
		{"key garbage", `{"image_id":"ubuntu","ssh_public_key":"ssh-ed25519 AAAA"}`, map[string]string{"ssh_public_key": "invalid_format"}},
		{"key private", `{"image_id":"ubuntu","ssh_public_key":"-----BEGIN OPENSSH PRIVATE KEY-----"}`, map[string]string{"ssh_public_key": "invalid_format"}},
		{"key options", `{"image_id":"ubuntu","ssh_public_key":"command=\"id\" ` + k + `"}`, map[string]string{"ssh_public_key": "options_not_allowed"}},
		{"key newline", `{"image_id":"ubuntu","ssh_public_key":"` + k + `\n` + k + `"}`, map[string]string{"ssh_public_key": "invalid_format"}},
		{"key long", `{"image_id":"ubuntu","ssh_public_key":"` + k + ` ` + strings.Repeat("c", 2000) + `"}`, map[string]string{"ssh_public_key": "too_long"}},
	}
	for _, c := range cases {
		_, fe := ParseCreate([]byte(c.body), p)
		got := fieldMap(fe)
		if len(got) != len(c.want) {
			t.Errorf("%s: %v want %v", c.name, got, c.want)
			continue
		}
		for f, code := range c.want {
			if got[f] != code {
				t.Errorf("%s: %s=%q want %q (%v)", c.name, f, got[f], code, fe)
			}
		}
		for _, f := range fe {
			if f.Message == "" || strings.Contains(f.Message, "AAAA") {
				t.Errorf("%s: message %q", c.name, f.Message)
			}
		}
	}
}

func TestParseCreateKeyIsGenuineAndNormalized(t *testing.T) {
	p := testPolicy()
	k := genKey(t)
	got, fe := ParseCreate([]byte(`{"image_id":"ubuntu","ssh_public_key":"`+k+` me@laptop"}`), p)
	if len(fe) != 0 {
		t.Fatal(fe)
	}
	if got.SSHPublicKey != k {
		t.Fatalf("normalised %q want %q", got.SSHPublicKey, k)
	}
	// Unsupported feature: field error, never silently dropped.
	p.SuppliedSSHKey = false
	_, fe = ParseCreate([]byte(`{"image_id":"ubuntu","ssh_public_key":"`+k+`"}`), p)
	if fieldMap(fe)["ssh_public_key"] != "unsupported" {
		t.Fatal(fe)
	}
	p.TTL = false
	_, fe = ParseCreate([]byte(`{"image_id":"ubuntu","ttl_seconds":60}`), p)
	if fieldMap(fe)["ttl_seconds"] != "unsupported" {
		t.Fatal(fe)
	}
}

func TestFieldErrorsJSONShape(t *testing.T) {
	_, fe := ParseCreate([]byte(`{}`), testPolicy())
	b, _ := json.Marshal(fe)
	if string(b) != `[{"field":"image_id","code":"required","message":"image_id is required"}]` {
		t.Fatal(string(b))
	}
}

func TestPolicyValidate(t *testing.T) {
	bad := []Policy{
		{DefaultProfile: "small", DefaultTTLSeconds: 0, MaxTTLSeconds: 86400, TTL: true},
		{DefaultProfile: "small", DefaultTTLSeconds: 90000, MaxTTLSeconds: 86400, TTL: true},
		{DefaultProfile: "small", DefaultTTLSeconds: 3600, MaxTTLSeconds: 40 * 86400, TTL: true},
		{DefaultProfile: "", DefaultTTLSeconds: 3600, MaxTTLSeconds: 86400, TTL: true},
		{DefaultProfile: "Bad Profile", DefaultTTLSeconds: 3600, MaxTTLSeconds: 86400, TTL: true},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%d accepted", i)
		}
	}
	if err := testPolicy().Validate(); err != nil {
		t.Fatal(err)
	}
}
