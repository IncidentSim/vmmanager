package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

type fakeKeys struct {
	owner int64
	vm    string
	err   error
	calls int
}

func (f *fakeKeys) GeneratedKey(_ context.Context, owner int64, vmID string) (GeneratedKey, error) {
	f.calls++
	if f.err != nil {
		return GeneratedKey{}, f.err
	}
	if owner != f.owner || vmID != f.vm {
		return GeneratedKey{}, vmbot.ErrNoVM
	}
	return GeneratedKey{PrivateKeyPEM: []byte("-----BEGIN TEST KEY-----\nabc\n-----END TEST KEY-----\n"), PublicKey: "ssh-ed25519 AAAATEST"}, nil
}

func optSetup(t *testing.T, o Options) (*httptest.Server, *vmbot.Controller, *vmlibtest.Backend) {
	t.Helper()
	st, e := vmbot.OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	b := vmlibtest.New(4, "ubuntu")
	b.SetCapabilities(vmlib.Capabilities{MaxVMs: 4, Profiles: []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 500, DiskMiB: 2048}, {Name: "big", VCPU: 2, MemMiB: 1024, DiskMiB: 4096}}})
	svc, e := vmlib.NewService(b)
	if e != nil {
		t.Fatal(e)
	}
	c := vmbot.NewController(svc, st, &terms{}, vmbot.ControllerConfig{Profile: "small", PublicOrigin: "https://terminal.test", OpTimeout: time.Second})
	c.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11, 22}, SharedImages: []string{"ubuntu"}, MaxVMs: 4, MaxVMsPerUser: 2})
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	h, e := New(c, []Credential{{SHA256: digest("one"), UserID: 11}, {SHA256: digest("two"), UserID: 22}})
	if e != nil {
		t.Fatal(e)
	}
	if e = h.Configure(o); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); c.Close(); st.Close() })
	return srv, c, b
}

func rawCall(t *testing.T, s *httptest.Server, method, path, token, key, body string) (*http.Response, []byte) {
	t.Helper()
	r, _ := http.NewRequest(method, s.URL+path, bytes.NewBufferString(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	p, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Body.Close()
	b, _ := io.ReadAll(p.Body)
	return p, b
}

func TestMethodNotAllowedAdvertisesAllow(t *testing.T) {
	s, _, _ := optSetup(t, Options{})
	for _, tc := range []struct{ method, path, allow string }{
		{"PUT", "/v1/vms", "GET, POST"},
		{"DELETE", "/v1/vms", "GET, POST"},
		{"POST", "/v1/vms/bot-x", "DELETE, GET"},
		{"GET", "/v1/vms/bot-x/stop", "POST"},
		{"DELETE", "/v1/vms/bot-x/ssh", "GET"},
		{"POST", "/v1/vms/bot-x/ssh-key", "GET"},
		{"POST", "/v1/capabilities", "GET"},
		{"POST", "/v1/operations/op-x", "GET"},
		{"PUT", "/v1/images/img-x", "DELETE, GET"},
		{"POST", "/v1/images", "GET"},
	} {
		resp, body := rawCall(t, s, tc.method, tc.path, "one", "k", `{}`)
		if resp.StatusCode != 405 || resp.Header.Get("Allow") != tc.allow || !strings.Contains(string(body), `"method_not_allowed"`) {
			t.Errorf("%s %s: %d allow=%q %s", tc.method, tc.path, resp.StatusCode, resp.Header.Get("Allow"), body)
		}
	}
	// Unknown routes stay 404; unauthenticated callers learn nothing about routes.
	if resp, _ := rawCall(t, s, "GET", "/v1/nope", "one", "", ""); resp.StatusCode != 404 {
		t.Error(resp.StatusCode)
	}
	if resp, _ := rawCall(t, s, "PUT", "/v1/vms", "bad", "", ""); resp.StatusCode != 401 || resp.Header.Get("Allow") != "" {
		t.Error(resp.StatusCode)
	}
}

func TestCreateValidationEnvelope(t *testing.T) {
	s, c, _ := optSetup(t, Options{})
	resp, body := rawCall(t, s, "POST", "/v1/vms", "one", "k1", `{"image_id":"ubuntu","resources":{"vcpu":1,"memory_mib":64},"zzz":1,"ttl_seconds":"x"}`)
	if resp.StatusCode != 422 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var env struct {
		Error struct {
			Code    string       `json:"code"`
			Message string       `json:"message"`
			Fields  []FieldError `json:"fields"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Code != "validation_failed" || env.Error.Message == "" {
		t.Fatalf("%s", body)
	}
	got := fieldMap(env.Error.Fields)
	if got["resources"] != "unknown_field" || got["zzz"] != "unknown_field" || got["ttl_seconds"] != "invalid_type" {
		t.Fatalf("%v", got)
	}
	if n := len(c.ControlVMs(11)); n != 0 {
		t.Fatal("validation failure created state")
	}
	// Missing/duplicated idempotency key is a field error too.
	resp, body = rawCall(t, s, "POST", "/v1/vms", "one", "", `{"image_id":"ubuntu"}`)
	if resp.StatusCode != 422 || !strings.Contains(string(body), `"Idempotency-Key"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestCreateProfileSelection(t *testing.T) {
	s, c, _ := optSetup(t, Options{})
	resp, body := rawCall(t, s, "POST", "/v1/vms", "one", "k1", `{"image_id":"ubuntu","profile":"nope"}`)
	if resp.StatusCode != 422 || !strings.Contains(string(body), `"profile_unknown"`) || !strings.Contains(string(body), `"profile"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if n := len(c.ControlVMs(11)); n != 0 {
		t.Fatal("unknown profile created state")
	}
	resp, body = rawCall(t, s, "POST", "/v1/vms", "one", "k2", `{"image_id":"ubuntu","profile":"big"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, body = rawCall(t, s, "POST", "/v1/vms", "one", "k3", `{"image_id":"ubuntu"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	vms := c.ControlVMs(11)
	if len(vms) != 2 || vms[0].Profile != "big" || vms[1].Profile != "small" {
		t.Fatalf("%+v", vms)
	}
}

func TestSuppliedKeyAndTTLRejectedUntilWired(t *testing.T) {
	s, _, _ := optSetup(t, Options{})
	k := genKey(t)
	for _, body := range []string{`{"image_id":"ubuntu","ssh_public_key":"` + k + `"}`, `{"image_id":"ubuntu","ttl_seconds":60}`} {
		resp, b := rawCall(t, s, "POST", "/v1/vms", "one", "k", body)
		if resp.StatusCode != 422 || !strings.Contains(string(b), `"unsupported"`) {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	}
	_, caps := call(t, s, "GET", "/v1/capabilities", "one", "", "", "")
	if caps["ssh_public_key_supported"] != false {
		t.Fatal(caps)
	}
	if _, ok := caps["ttl"]; ok {
		t.Fatalf("TTL advertised without implementation: %v", caps)
	}
}

func TestCapabilitiesAdvertiseConfiguredFeatures(t *testing.T) {
	keys := &fakeKeys{}
	s, _, _ := optSetup(t, Options{Policy: Policy{DefaultProfile: "small", DefaultTTLSeconds: 3600, MaxTTLSeconds: 86400, TTL: true, SuppliedSSHKey: true, GeneratedSSHKey: true}, Keys: keys})
	_, caps := call(t, s, "GET", "/v1/capabilities", "one", "", "", "")
	ttl, _ := caps["ttl"].(map[string]any)
	if caps["default_profile"] != "small" || caps["ssh_public_key_supported"] != true || caps["ssh_generated_key_supported"] != true ||
		ttl["default_seconds"] != float64(3600) || ttl["max_seconds"] != float64(86400) || caps["max_vms"] != float64(4) || caps["max_vms_per_user"] != float64(2) {
		t.Fatalf("%v", caps)
	}
	// A generated-key flag without a provider is a configuration error, not a lie.
	st, _, _ := optSetup(t, Options{})
	_ = st
	h := &Server{}
	if err := h.Configure(Options{Policy: Policy{DefaultProfile: "small", GeneratedSSHKey: true}}); err == nil {
		t.Fatal("GeneratedSSHKey without provider accepted")
	}
}

func TestSSHKeyEndpoint(t *testing.T) {
	keys := &fakeKeys{owner: 11}
	s, _, _ := optSetup(t, Options{Policy: Policy{DefaultProfile: "small", GeneratedSSHKey: true}, Keys: keys})
	_, a := call(t, s, "POST", "/v1/vms", "one", "k1", `{"image_id":"ubuntu"}`, "")
	vm := a["vm_id"].(string)
	keys.vm = vm
	resp, body := rawCall(t, s, "GET", "/v1/vms/"+vm+"/ssh-key", "one", "", "")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") || !strings.Contains(string(body), "BEGIN TEST KEY") {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if out["vm_id"] != vm || out["public_key"] != "ssh-ed25519 AAAATEST" || out["private_key"] == "" {
		t.Fatalf("%v", out)
	}
	// Foreign and unknown are identical and never reach the provider.
	before := keys.calls
	r1, b1 := rawCall(t, s, "GET", "/v1/vms/"+vm+"/ssh-key", "two", "", "")
	r2, b2 := rawCall(t, s, "GET", "/v1/vms/bot-doesnotexist/ssh-key", "two", "", "")
	if r1.StatusCode != 404 || r2.StatusCode != 404 || string(b1) != string(b2) || keys.calls != before {
		t.Fatalf("%d %d %s %s %d", r1.StatusCode, r2.StatusCode, b1, b2, keys.calls)
	}
	if strings.Contains(string(b1), "BEGIN") {
		t.Fatal("leak")
	}
	// Supplied-key VM: explicit key_not_generated.
	keys.err = ErrKeyNotGenerated
	resp, body = rawCall(t, s, "GET", "/v1/vms/"+vm+"/ssh-key", "one", "", "")
	if resp.StatusCode != 409 || !strings.Contains(string(body), `"key_not_generated"`) || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	keys.err = errors.New("secret path /var/lib/x")
	resp, body = rawCall(t, s, "GET", "/v1/vms/"+vm+"/ssh-key", "one", "", "")
	if resp.StatusCode != 503 || strings.Contains(string(body), "/var/lib") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestSSHKeyEndpointWithoutProviderNeverFakesKey(t *testing.T) {
	s, _, _ := optSetup(t, Options{})
	_, a := call(t, s, "POST", "/v1/vms", "one", "k1", `{"image_id":"ubuntu"}`, "")
	resp, body := rawCall(t, s, "GET", "/v1/vms/"+a["vm_id"].(string)+"/ssh-key", "one", "", "")
	if resp.StatusCode != 501 || !strings.Contains(string(body), `"not_implemented"`) || strings.Contains(string(body), "PRIVATE") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	// Even without a provider an unowned ID is the ordinary 404.
	if resp, _ = rawCall(t, s, "GET", "/v1/vms/"+a["vm_id"].(string)+"/ssh-key", "two", "", ""); resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
}

func TestErrorMapperHook(t *testing.T) {
	errExpired := errors.New("expired internal detail")
	s, c, _ := optSetup(t, Options{ErrorMapper: func(e error) (int, string, bool) {
		if errors.Is(e, errExpired) {
			return 409, "vm_expired", true
		}
		return 0, "", false
	}})
	_ = c
	w := httptest.NewRecorder()
	fail2 := (&Server{opts: Options{ErrorMapper: func(e error) (int, string, bool) { return 409, "vm_expired", errors.Is(e, errExpired) }}})
	fail2.fail(w, errExpired)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"vm_expired"`) || strings.Contains(w.Body.String(), "internal detail") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	_ = s
}
