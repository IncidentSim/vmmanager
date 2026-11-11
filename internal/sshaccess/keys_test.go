package sshaccess

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestGenerateEd25519RoundTrips(t *testing.T) {
	priv, pub, err := GenerateEd25519()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("private key is not a valid OpenSSH key: %v", err)
	}
	if signer.PublicKey().Type() != "ssh-ed25519" {
		t.Fatal(signer.PublicKey().Type())
	}
	canon, err := ParsePublicKey(pub)
	if err != nil || canon != pub {
		t.Fatalf("generated public key must be canonical: %q %v", canon, err)
	}
	if strings.Contains(pub, "\n") || strings.Contains(pub, " ") && len(strings.Fields(pub)) != 2 {
		t.Fatalf("public key must be one type+base64 line: %q", pub)
	}
	priv2, pub2, _ := GenerateEd25519()
	if string(priv) == string(priv2) || pub == pub2 {
		t.Fatal("keys must be random")
	}
}

func TestParsePublicKey(t *testing.T) {
	_, good, _ := GenerateEd25519()
	b64 := strings.Fields(good)[1]
	cases := []struct {
		name, in string
		ok       bool
	}{
		{"plain", good, true},
		{"comment is dropped", good + " alice@laptop", true},
		{"trailing newline", good + "\n", true},
		{"empty", "", false},
		{"garbage", "ssh-ed25519 notbase64!!", false},
		{"wrong blob", "ssh-ed25519 AAAAB3NzaC1yc2EAAAADAQABAAAAgQ", false},
		{"type mismatch", "ssh-rsa " + b64, false},
		{"two lines", good + "\n" + good, false},
		{"options", `command="id" ` + good, false},
		{"nul", good + "\x00", false},
		{"cr injection", good + "\r\nssh-rsa AAAA", false},
		{"too long", good + " " + strings.Repeat("a", 2000), false},
		{"private key", "-----BEGIN OPENSSH PRIVATE KEY-----", false},
	}
	for _, c := range cases {
		got, err := ParsePublicKey(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v", c.name, err)
			continue
		}
		if !c.ok && !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: want ErrInvalidKey, got %v", c.name, err)
		}
		if c.ok && got != good {
			t.Errorf("%s: not canonicalised: %q", c.name, got)
		}
	}
}

func TestParsePublicKeyRejectsCertificatesAndWeakRSA(t *testing.T) {
	// 1024-bit RSA public key generated offline; must be refused as weak.
	if _, err := ParsePublicKey(weakRSA1024); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("weak rsa accepted: %v", err)
	}
}
