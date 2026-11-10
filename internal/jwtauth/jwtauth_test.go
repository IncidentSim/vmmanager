package jwtauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	iss = "test-issuer"
	aud = "test-audience"
)

var now = time.Unix(1_800_000_000, 0)

func genKey(t *testing.T, bits int) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func writePair(t *testing.T, dir string, k *rsa.PrivateKey) (priv, pub string) {
	t.Helper()
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	priv = filepath.Join(dir, "jwt-private.pem")
	if err := os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	pk, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	pub = filepath.Join(dir, "jwt-public.pem")
	if err := os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pk}), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

func pair(t *testing.T) (*Signer, *Verifier, *rsa.PrivateKey) {
	t.Helper()
	k := genKey(t, 2048)
	priv, pub := writePair(t, t.TempDir(), k)
	s, err := LoadSigner(priv, iss, aud)
	if err != nil {
		t.Fatal(err)
	}
	v, err := LoadVerifier(pub, iss, aud)
	if err != nil {
		t.Fatal(err)
	}
	return s, v, k
}

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	s, v, _ := pair(t)
	tok, exp, err := s.Issue(4242, "@Alice_Example", DefaultTTL, now)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(now.Add(8*time.Hour)) || DefaultTTL != 28800*time.Second {
		t.Fatalf("default TTL must be 8h: %v", exp)
	}
	c, err := v.Verify(tok, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != 4242 || c.Username != "alice_example" || !c.ExpiresAt.Equal(exp) {
		t.Fatalf("claims %+v", c)
	}
	// No username claim when none is given.
	tok2, _, err := s.Issue(7, "", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if c, err = v.Verify(tok2, now); err != nil || c.Username != "" || c.Subject != 7 {
		t.Fatalf("%+v %v", c, err)
	}
	// Header must be RS256 and the token never contains the user name in clear? (it is a signed claim, not secret)
	hdr := strings.SplitN(tok, ".", 2)[0]
	raw, _ := base64.RawURLEncoding.DecodeString(hdr)
	if !strings.Contains(string(raw), `"RS256"`) {
		t.Fatalf("header %s", raw)
	}
}

func TestIssueRejectsBadInputs(t *testing.T) {
	s, _, _ := pair(t)
	for _, c := range []struct {
		id   int64
		name string
		ttl  time.Duration
	}{{0, "", time.Hour}, {-1, "", time.Hour}, {5, "bad name", time.Hour}, {5, "", 0}, {5, "", -time.Second}, {5, "", MaxTTL + time.Second}} {
		if _, _, err := s.Issue(c.id, c.name, c.ttl, now); err == nil {
			t.Errorf("must reject %+v", c)
		}
	}
}

func sign(t *testing.T, method jwt.SigningMethod, key any, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func base(extra map[string]any) jwt.MapClaims {
	c := jwt.MapClaims{"iss": iss, "aud": aud, "sub": "99", "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func TestVerifierRejectsInvalidTokens(t *testing.T) {
	_, v, k := pair(t)
	other := genKey(t, 2048)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubDER, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)

	good := sign(t, jwt.SigningMethodRS256, k, base(nil))
	if _, err := v.Verify(good, now); err != nil {
		t.Fatalf("baseline must verify: %v", err)
	}
	none := sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, base(nil))
	parts := strings.Split(good, ".")
	hs256WithPublicKey := sign(t, jwt.SigningMethodHS256, pubDER, base(nil))
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+iss+`","aud":"`+aud+`","sub":"1","iat":1800000000,"nbf":1800000000,"exp":1800003600}`)) + "." + parts[2]

	cases := map[string]string{
		"empty":        "",
		"garbage":      "not-a-jwt",
		"none":         none,
		"hs256":        hs256WithPublicKey,
		"hs256 pubkey": sign(t, jwt.SigningMethodHS256, []byte("secret"), base(nil)),
		"es256":        sign(t, jwt.SigningMethodES256, ec, base(nil)),
		"rs384":        sign(t, jwt.SigningMethodRS384, k, base(nil)),
		"ps256":        sign(t, jwt.SigningMethodPS256, k, base(nil)),
		"wrong key":    sign(t, jwt.SigningMethodRS256, other, base(nil)),
		"tampered":     tampered,
		"expired":      sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"exp": now.Add(-time.Minute).Unix()})),
		"no exp":       sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"exp": nil})),
		"not yet":      sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"nbf": now.Add(time.Hour).Unix()})),
		"no nbf":       sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"nbf": nil})),
		"no iat":       sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"iat": nil})),
		"future iat":   sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"iat": now.Add(time.Hour).Unix()})),
		"long life":    sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"exp": now.Add(MaxTTL + 2*time.Hour).Unix()})),
		"wrong iss":    sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"iss": "evil"})),
		"no iss":       sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"iss": nil})),
		"wrong aud":    sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"aud": "evil"})),
		"no aud":       sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"aud": nil})),
		"no sub":       sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"sub": nil})),
		"sub zero":     sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"sub": "0"})),
		"sub negative": sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"sub": "-5"})),
		"sub leading0": sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"sub": "099"})),
		"sub plus":     sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"sub": "+99"})),
		"sub number":   sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"sub": 99})),
		"sub spaced":   sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"sub": "99 "})),
		"sub overflow": sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"sub": "99999999999999999999"})),
		"bad username": sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"tg_username": "bad name"})),
		"upper uname":  sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"tg_username": "Alice_Example"})),
		"at uname":     sign(t, jwt.SigningMethodRS256, k, base(map[string]any{"tg_username": "@alice_example"})),
		"oversize":     good + strings.Repeat("A", MaxTokenLength),
		"bearer word":  "Bearer " + good,
		"whitespace":   good + "\n",
	}
	for name, tok := range cases {
		if c, err := v.Verify(tok, now); err == nil {
			t.Errorf("%s: must be rejected, got %+v", name, c)
		}
	}
}

func TestVerifierKeyTypeMismatch(t *testing.T) {
	dir := t.TempDir()
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&ec.PublicKey)
	p := filepath.Join(dir, "ec.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVerifier(p, iss, aud); err == nil {
		t.Fatal("EC public key must be rejected")
	}
}

func TestKeyLoadingIsStrict(t *testing.T) {
	dir := t.TempDir()
	k := genKey(t, 2048)
	priv, pub := writePair(t, dir, k)

	// Verifier needs only the public key; a private key file is not a public key.
	if _, err := LoadVerifier(priv, iss, aud); err == nil {
		t.Error("verifier must refuse private key material")
	}
	if _, err := LoadVerifier(filepath.Join(dir, "missing.pem"), iss, aud); err == nil {
		t.Error("missing public key")
	}
	if _, err := LoadVerifier("relative.pem", iss, aud); err == nil {
		t.Error("relative path")
	}
	if _, err := LoadVerifier(pub, "", aud); err == nil {
		t.Error("issuer required")
	}
	if _, err := LoadVerifier(pub, iss, ""); err == nil {
		t.Error("audience required")
	}
	// Signer: private file must be private.
	if err := os.Chmod(priv, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(priv, iss, aud); err == nil {
		t.Error("group readable private key must be refused")
	}
	if err := os.Chmod(priv, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(pub, iss, aud); err == nil {
		t.Error("signer must refuse a public key file")
	}
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(priv, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(link, iss, aud); err == nil {
		t.Error("symlinked private key must be refused")
	}
	// Small RSA keys are refused.
	small := genKey(t, 1024)
	sp, spub := writePair(t, t.TempDir(), small)
	if _, err := LoadSigner(sp, iss, aud); err == nil {
		t.Error("1024-bit signer key must be refused")
	}
	if _, err := LoadVerifier(spub, iss, aud); err == nil {
		t.Error("1024-bit verifier key must be refused")
	}
	// PKCS#1 PEM is accepted for convenience.
	p1 := filepath.Join(t.TempDir(), "k1.pem")
	if err := os.WriteFile(p1, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(p1, iss, aud); err != nil {
		t.Errorf("PKCS#1 key: %v", err)
	}
}

func TestErrorsNeverEchoTokenOrKey(t *testing.T) {
	_, v, _ := pair(t)
	tok := "secret-looking.token.value"
	_, err := v.Verify(tok, now)
	if err == nil || strings.Contains(err.Error(), "secret-looking") {
		t.Fatalf("error must not echo input: %v", err)
	}
}
