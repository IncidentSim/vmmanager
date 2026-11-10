package controlapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type terms struct{}

func (*terms) Start(context.Context, vmbot.TerminalTarget) error { return nil }
func (*terms) Revoke(string)                                     {}
func (*terms) Close()                                            {}
func digest(s string) string                                     { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func setup(t *testing.T) (*httptest.Server, *vmbot.Controller) {
	t.Helper()
	st, e := vmbot.OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	svc, e := vmlib.NewService(vmlibtest.New(4, "ubuntu"))
	if e != nil {
		t.Fatal(e)
	}
	c := vmbot.NewController(svc, st, &terms{}, vmbot.ControllerConfig{Profile: "small", PublicOrigin: "https://terminal.test", OpTimeout: time.Second})
	c.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11, 22}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	h, e := New(c, []Credential{{SHA256: digest("one"), UserID: 11}, {SHA256: digest("two"), UserID: 22}, {SHA256: digest("bot"), Service: true}})
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); c.Close(); st.Close() })
	return srv, c
}
func call(t *testing.T, s *httptest.Server, method, path, token, key, body, acting string) (int, map[string]any) {
	t.Helper()
	r, _ := http.NewRequest(method, s.URL+path, bytes.NewBufferString(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if acting != "" {
		r.Header.Set("X-Acting-User-ID", acting)
	}
	p, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Body.Close()
	var b map[string]any
	if e = json.NewDecoder(p.Body).Decode(&b); e != nil {
		t.Fatal(e)
	}
	return p.StatusCode, b
}
func TestAuthenticatedHTTPContract(t *testing.T) {
	s, c := setup(t)
	for _, tc := range []struct {
		token, acting string
		code          int
	}{{"", "", 401}, {"bad", "", 401}, {"one", "22", 403}, {"bot", "", 403}, {"bot", "022", 403}, {"bot", "33", 403}, {"bot", "22", 200}, {"one", "", 200}} {
		code, _ := call(t, s, "GET", "/v1/vms", tc.token, "", "", tc.acting)
		if code != tc.code {
			t.Fatalf("auth code %d want %d", code, tc.code)
		}
	}
	code, a := call(t, s, "POST", "/v1/vms", "one", "create-1", `{"name":"lab","image_id":"ubuntu"}`, "")
	if code != 202 {
		t.Fatalf("create %d %v", code, a)
	}
	vm := a["vm_id"].(string)
	op := a["operation_id"].(string)
	code, b := call(t, s, "POST", "/v1/vms", "one", "create-1", `{"image_id":"ubuntu","name":"lab"}`, "")
	if code != 202 || b["vm_id"] != vm || b["operation_id"] != op {
		t.Fatalf("duplicate %d %v", code, b)
	}
	code, _ = call(t, s, "POST", "/v1/vms", "one", "create-1", `{"name":"else","image_id":"ubuntu"}`, "")
	if code != 409 {
		t.Fatal(code)
	}
	for _, p := range []string{"/v1/vms/" + vm, "/v1/vms/" + vm + "/ssh", "/v1/operations/" + op} {
		code, _ = call(t, s, "GET", p, "two", "", "", "")
		if code != 404 {
			t.Fatalf("foreign %s %d", p, code)
		}
	}
	for _, body := range []string{`{"owner_user_id":22,"image_id":"ubuntu"}`, `{"image_id":"ubuntu"} {}`, `{"image_id":"ubuntu","ssh_public_key":"ssh-ed25519 AAA"}`} {
		code, _ = call(t, s, "POST", "/v1/vms", "two", "bad", body, "")
		if code != 422 {
			t.Fatalf("strict %d", code)
		}
	}
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		o, _ := c.ControlOperation(11, op)
		if o.Status == "succeeded" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	code, b = call(t, s, "GET", "/v1/operations/"+op, "one", "", "", "")
	if code != 200 || b["status"] != "succeeded" {
		t.Fatalf("poll %d %v", code, b)
	}
	code, _ = call(t, s, "POST", "/v1/vms/"+vm+"/stop", "one", "stop-1", `{}`, "")
	if code != 202 {
		t.Fatal(code)
	}
	if _, e := c.ControlTerminalURL(11, vm); e == nil {
		t.Fatal("stop must fence terminal before enqueue")
	}
	code, _ = call(t, s, "DELETE", "/v1/vms/"+vm, "two", "evil", "", "")
	if code != 404 {
		t.Fatal(code)
	}
}
