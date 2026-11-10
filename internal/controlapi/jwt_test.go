package controlapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/petruekhin/vibe-firecracker/internal/identity"
	"github.com/petruekhin/vibe-firecracker/internal/jwtauth"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

const (
	testIss = "test-bot"
	testAud = "test-api"
)

type jenv struct {
	srv    *httptest.Server
	ctl    *vmbot.Controller
	signer *jwtauth.Signer
	key    *rsa.PrivateKey
	dir    string
	jwt    *JWTAuth
}

func newKeyFiles(t *testing.T, dir string) (priv, pub string, k *rsa.PrivateKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	priv = filepath.Join(dir, "private.pem")
	if err = os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	pk, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	pub = filepath.Join(dir, "public.pem")
	if err = os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pk}), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

// newJWT builds an API whose ONLY identity material is the public key, one
// legacy numeric ID (11) and configured usernames; static credentials are
// optional via creds.
func newJWT(t *testing.T, usernames []string, creds []Credential) *jenv {
	t.Helper()
	dir := t.TempDir()
	priv, pub, k := newKeyFiles(t, dir)
	signer, err := jwtauth.LoadSigner(priv, testIss, testAud)
	if err != nil {
		t.Fatal(err)
	}
	ver, err := jwtauth.LoadVerifier(pub, testIss, testAud)
	if err != nil {
		t.Fatal(err)
	}
	st, err := vmbot.OpenControlStore(filepath.Join(dir, "state"), 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := vmlib.NewService(vmlibtest.New(8, "ubuntu"))
	if err != nil {
		t.Fatal(err)
	}
	c := vmbot.NewController(svc, st, &terms{}, vmbot.ControllerConfig{Profile: "small", PublicOrigin: "https://terminal.test", OpTimeout: time.Second})
	c.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 4, MaxVMsPerUser: 1})
	if err = c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := identity.OpenBindings(filepath.Join(dir, "state", "username-bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	canon, err := identity.CanonicalList(usernames)
	if err != nil {
		t.Fatal(err)
	}
	j := &JWTAuth{Verifier: ver, Bindings: b, Usernames: canon}
	h, err := NewWithJWT(c, creds, j)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); c.Close(); st.Close() })
	return &jenv{srv: srv, ctl: c, signer: signer, key: k, dir: dir, jwt: j}
}

func (e *jenv) token(t *testing.T, id int64, name string) string {
	t.Helper()
	tok, _, err := e.signer.Issue(id, name, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestJWTOnlyAPIWorksWithoutStaticCredentials(t *testing.T) {
	e := newJWT(t, []string{"@Alice_Example", "bob_example"}, nil)
	// legacy numeric allowlist user, no username claim
	if code, _ := call(t, e.srv, "GET", "/v1/vms", e.token(t, 11, ""), "", "", ""); code != 200 {
		t.Fatalf("legacy id token: %d", code)
	}
	// first contact of an allowed username binds and authorizes the real ID
	alice := e.token(t, 1001, "alice_example")
	if code, _ := call(t, e.srv, "GET", "/v1/vms", alice, "", "", ""); code != 200 {
		t.Fatalf("alice: %d", code)
	}
	if id, ok := e.jwt.Bindings.Owner("alice_example"); !ok || id != 1001 {
		t.Fatalf("binding %d %v", id, ok)
	}
	// once bound, a token without the username claim (same ID) still works
	if code, _ := call(t, e.srv, "GET", "/v1/vms", e.token(t, 1001, ""), "", "", ""); code != 200 {
		t.Fatalf("bound id without claim: %d", code)
	}
	// ...but an unbound, unlisted ID with no claim does not
	if code, _ := call(t, e.srv, "GET", "/v1/vms", e.token(t, 1003, ""), "", "", ""); code != 403 {
		t.Fatalf("unbound id: %d", code)
	}
	// recycled handle: another ID claiming the pinned name is refused
	if code, _ := call(t, e.srv, "GET", "/v1/vms", e.token(t, 1002, "alice_example"), "", "", ""); code != 403 {
		t.Fatalf("recycled handle: %d", code)
	}
	if id, _ := e.jwt.Bindings.Owner("alice_example"); id != 1001 {
		t.Fatal("binding was reassigned")
	}
	// username not configured: no bootstrap
	if code, _ := call(t, e.srv, "GET", "/v1/vms", e.token(t, 1004, "mallory_example"), "", "", ""); code != 403 {
		t.Fatalf("unlisted: %d", code)
	}
	if _, ok := e.jwt.Bindings.Owner("mallory_example"); ok {
		t.Fatal("unlisted name must never be bound")
	}
	// a legacy ID claiming a pinned name of someone else is a mismatch too
	if code, _ := call(t, e.srv, "GET", "/v1/vms", e.token(t, 11, "alice_example"), "", "", ""); code != 403 {
		t.Fatalf("legacy id with foreign pinned name: %d", code)
	}
}

func TestJWTRejectsInvalidBearersWith401(t *testing.T) {
	e := newJWT(t, []string{"alice_example"}, nil)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	mk := func(m jwt.SigningMethod, key any, mut func(jwt.MapClaims)) string {
		c := jwt.MapClaims{"iss": testIss, "aud": testAud, "sub": "11", "iat": time.Now().Unix(), "nbf": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
		if mut != nil {
			mut(c)
		}
		s, err := jwt.NewWithClaims(m, c).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for name, tok := range map[string]string{
		"expired":   mk(jwt.SigningMethodRS256, e.key, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"wrong key": mk(jwt.SigningMethodRS256, other, nil),
		"hs256":     mk(jwt.SigningMethodHS256, []byte("x"), nil),
		"none":      mk(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, nil),
		"issuer":    mk(jwt.SigningMethodRS256, e.key, func(c jwt.MapClaims) { c["iss"] = "evil" }),
		"audience":  mk(jwt.SigningMethodRS256, e.key, func(c jwt.MapClaims) { c["aud"] = "evil" }),
		"subject":   mk(jwt.SigningMethodRS256, e.key, func(c jwt.MapClaims) { c["sub"] = "011" }),
		// a forged username claim in a token signed by someone else must never bind
		"forged username": mk(jwt.SigningMethodRS256, other, func(c jwt.MapClaims) { c["sub"] = "5555"; c["tg_username"] = "alice_example" }),
		"garbage":         "abc.def.ghi",
	} {
		if code, _ := call(t, e.srv, "GET", "/v1/vms", tok, "", "", ""); code != 401 {
			t.Errorf("%s: %d want 401", name, code)
		}
	}
	if _, ok := e.jwt.Bindings.Owner("alice_example"); ok {
		t.Fatal("forged username claim bound a slot")
	}
	if code, _ := call(t, e.srv, "GET", "/v1/vms", "", "", "", ""); code != 401 {
		t.Fatal("missing bearer")
	}
}

func TestJWTUserCannotChooseActingUserOrBypass(t *testing.T) {
	e := newJWT(t, []string{"alice_example"}, nil)
	alice := e.token(t, 1001, "alice_example")
	for _, acting := range []string{"11", "1001", "99"} {
		if code, _ := call(t, e.srv, "GET", "/v1/vms", alice, "", "", acting); code != 403 {
			t.Fatalf("acting header %q must be refused for a user token: %d", acting, code)
		}
	}
	// Alice's VM is invisible to another user: identical 404.
	code, a := call(t, e.srv, "POST", "/v1/vms", alice, "k1", `{"name":"lab","image_id":"ubuntu"}`, "")
	if code != 202 {
		t.Fatalf("create: %d %v", code, a)
	}
	vm := a["vm_id"].(string)
	if code, _ = call(t, e.srv, "GET", "/v1/vms/"+vm, e.token(t, 11, ""), "", "", ""); code != 404 {
		t.Fatalf("foreign VM must be 404: %d", code)
	}
	if code, _ = call(t, e.srv, "GET", "/v1/vms/"+vm, alice, "", "", ""); code != 200 {
		t.Fatalf("own VM: %d", code)
	}
	// Owner derives from the verified sub; a JSON owner field is an unknown field.
	if code, _ = call(t, e.srv, "POST", "/v1/vms", alice, "k2", `{"image_id":"ubuntu","owner_user_id":11}`, ""); code != 422 {
		t.Fatalf("owner in body: %d", code)
	}
	// quota: one VM per user
	if code, _ = call(t, e.srv, "POST", "/v1/vms", alice, "k3", `{"name":"two","image_id":"ubuntu"}`, ""); code != 429 {
		t.Fatalf("per-user quota: %d", code)
	}
	// idempotent replay of the first request
	code, b := call(t, e.srv, "POST", "/v1/vms", alice, "k1", `{"image_id":"ubuntu","name":"lab"}`, "")
	if code != 202 || b["vm_id"] != vm {
		t.Fatalf("replay: %d %v", code, b)
	}
}

func TestRevokedUsernameBlocksAccessButKeepsVMs(t *testing.T) {
	e := newJWT(t, []string{"alice_example"}, nil)
	alice := e.token(t, 1001, "alice_example")
	code, a := call(t, e.srv, "POST", "/v1/vms", alice, "k1", `{"name":"lab","image_id":"ubuntu"}`, "")
	if code != 202 {
		t.Fatalf("create %d", code)
	}
	vm := a["vm_id"].(string)
	if !e.ctl.ControlAllowed(1001) {
		t.Fatal("bound configured user must be allowed (projection fences)")
	}
	e.jwt.SetUsernames(nil) // operator removes the username from the config (restart)
	if e.ctl.ControlAllowed(1001) {
		t.Fatal("removed username must lose access, including restart fences")
	}
	if code, _ = call(t, e.srv, "GET", "/v1/vms", alice, "", "", ""); code != 403 {
		t.Fatalf("revoked: %d", code)
	}
	e.jwt.SetUsernames([]string{"alice_example"})
	if code, b := call(t, e.srv, "GET", "/v1/vms/"+vm, alice, "", "", ""); code != 200 || b["id"] != vm {
		t.Fatalf("VM must survive revocation: %d %v", code, b)
	}
}

func TestConcurrentFirstClaimThroughAPIHasOneWinner(t *testing.T) {
	e := newJWT(t, []string{"race_example"}, nil)
	var wg sync.WaitGroup
	codes := make([]int, 16)
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, _ := http.NewRequest("GET", e.srv.URL+"/v1/vms", nil)
			tok, _, _ := e.signer.Issue(int64(3000+i), "race_example", time.Hour, time.Now())
			r.Header.Set("Authorization", "Bearer "+tok)
			p, err := http.DefaultClient.Do(r)
			if err == nil {
				codes[i] = p.StatusCode
				p.Body.Close()
			}
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == 200 {
			ok++
		} else if c != 403 {
			t.Fatalf("unexpected %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one ID may win the slot, got %d", ok)
	}
}

func TestStaticCredentialsStillWorkNextToJWT(t *testing.T) {
	e := newJWT(t, []string{"alice_example"}, []Credential{{SHA256: digest("one"), UserID: 11}, {SHA256: digest("bot"), Service: true}})
	if code, _ := call(t, e.srv, "GET", "/v1/vms", "one", "", "", ""); code != 200 {
		t.Fatalf("static user: %d", code)
	}
	if code, _ := call(t, e.srv, "GET", "/v1/vms", "one", "", "", "22"); code != 403 {
		t.Fatalf("static user acting header: %d", code)
	}
	if code, _ := call(t, e.srv, "GET", "/v1/vms", "bot", "", "", "11"); code != 200 {
		t.Fatalf("service delegation: %d", code)
	}
	if code, _ := call(t, e.srv, "GET", "/v1/vms", "bot", "", "", ""); code != 403 {
		t.Fatalf("service without acting: %d", code)
	}
	// Service delegation can act for a bound username owner too.
	if code, _ := call(t, e.srv, "GET", "/v1/vms", e.token(t, 1001, "alice_example"), "", "", ""); code != 200 {
		t.Fatal("bind alice")
	}
	if code, _ := call(t, e.srv, "GET", "/v1/vms", "bot", "", "", "1001"); code != 200 {
		t.Fatalf("service acting for bound user: %d", code)
	}
	if code, _ := call(t, e.srv, "GET", "/v1/vms", "bot", "", "", "1002"); code != 403 {
		t.Fatalf("service acting for unbound user: %d", code)
	}
}

func TestConstructorPolicyRequirements(t *testing.T) {
	st, _ := vmbot.OpenControlStore(t.TempDir()+"/state", 0)
	defer st.Close()
	svc, _ := vmlib.NewService(vmlibtest.New(2, "ubuntu"))
	c := vmbot.NewController(svc, st, &terms{}, vmbot.ControllerConfig{Profile: "small"})
	c.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11}, MaxVMs: 2, MaxVMsPerUser: 1})
	if _, err := New(c, nil); err == nil {
		t.Fatal("no credentials and no JWT must be rejected")
	}
	c2 := vmbot.NewController(svc, st, &terms{}, vmbot.ControllerConfig{Profile: "small"})
	c2.ConfigureControl(vmbot.ControlPolicy{MaxVMs: 2, MaxVMsPerUser: 1})
	if _, err := New(c2, []Credential{{SHA256: digest("x"), Service: true}}); err == nil {
		t.Fatal("no identity policy at all must be rejected")
	}
}

func TestConfigJWTAndUsernames(t *testing.T) {
	dir := t.TempDir()
	_, pub, _ := newKeyFiles(t, dir)
	write := func(body string) string {
		p := filepath.Join(dir, "c.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	jw := `"jwt":{"public_key_file":"` + pub + `","issuer":"incident-sim-bot","audience":"incident-sim-api"}`
	ok := `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["@Alice_Example","bob_example"],"shared_image_ids":["ubuntu"],"credentials":[],` + jw + `}`
	c, err := LoadConfig(write(ok))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedUsernames) != 2 || c.JWT == nil || c.JWT.Issuer != "incident-sim-bot" {
		t.Fatalf("%+v", c)
	}
	if n, err := c.CanonicalUsernames(); err != nil || n[0] != "alice_example" {
		t.Fatalf("%v %v", n, err)
	}
	if v, err := c.Verifier(); err != nil || v == nil {
		t.Fatalf("verifier: %v", err)
	}
	for name, body := range map[string]string{
		"dup usernames":     `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["alice_example","@ALICE_EXAMPLE"],"credentials":[],` + jw + `}`,
		"bad username":      `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["bad name"],"credentials":[],` + jw + `}`,
		"no identity":       `{"state_dir":"/unused","backend":` + testBackend + `,"credentials":[],` + jw + `}`,
		"no creds no jwt":   `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["alice_example"],"credentials":[]}`,
		"relative key":      `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["alice_example"],"credentials":[],"jwt":{"public_key_file":"k.pem","issuer":"a","audience":"b"}}`,
		"missing issuer":    `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["alice_example"],"credentials":[],"jwt":{"public_key_file":"` + pub + `","audience":"b"}}`,
		"missing audience":  `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["alice_example"],"credentials":[],"jwt":{"public_key_file":"` + pub + `","issuer":"b"}}`,
		"unknown jwt field": `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["alice_example"],"credentials":[],"jwt":{"public_key_file":"` + pub + `","issuer":"a","audience":"b","secret":"x"}}`,
		"expose w/o origin": `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["alice_example"],"credentials":[],"expose_api_on_terminal":true,` + jw + `}`,
	} {
		if _, err := LoadConfig(write(body)); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	// A private key can never be configured as the API verification key.
	priv := filepath.Join(dir, "private.pem")
	bad := `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_usernames":["alice_example"],"credentials":[],"jwt":{"public_key_file":"` + priv + `","issuer":"a","audience":"b"}}`
	if c, err := LoadConfig(write(bad)); err == nil {
		if _, err = c.Verifier(); err == nil {
			t.Fatal("private key accepted as API verification key")
		}
	}
	// legacy static-only config still loads unchanged
	legacy := `{"state_dir":"/unused","backend":` + testBackend + `,"allowed_user_ids":[11],"credentials":[{"sha256":"` + digest("x") + `","user_id":11}]}`
	if c, err := LoadConfig(write(legacy)); err != nil || c.JWT != nil {
		t.Fatalf("legacy config: %v", err)
	}
}
