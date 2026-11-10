package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const secret = "header.payload-test-only-secret.signature"

// fakeAPI is a minimal HTTP stand-in for the /v1 control API. It records the
// credential headers it received so tests can assert the CLI's auth behavior.
type fakeAPI struct {
	mu      sync.Mutex
	auth    []string
	acting  []string
	idemp   []string
	polls   int
	reqs    []string
	srv     *httptest.Server
	opState string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{opState: "succeeded"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.acting = append(f.acting, r.Header.Get("X-Acting-User-ID"))
		f.idemp = append(f.idemp, r.Header.Get("Idempotency-Key"))
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"code":"unauthorized"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/vms":
			w.WriteHeader(202)
			w.Write([]byte(`{"operation_id":"op1","vm_id":"vm1"}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/commit"):
			w.WriteHeader(202)
			w.Write([]byte(`{"operation_id":"op2","image_id":"img1"}`))
		case r.Method == "DELETE" || r.Method == "POST":
			w.WriteHeader(202)
			w.Write([]byte(`{"operation_id":"op3"}`))
		case strings.HasPrefix(r.URL.Path, "/v1/operations/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/operations/")
			f.polls++
			st := f.opState
			if f.polls == 1 && st == "succeeded" {
				st = "running"
			}
			w.Write([]byte(`{"id":"` + id + `","status":"` + st + `","kind":"x"}`))
		case r.URL.Path == "/v1/vms":
			w.Write([]byte(`{"vms":[{"id":"vm1","name":"n","image_id":"img","state":"running","phase":"active"}]}`))
		case strings.HasSuffix(r.URL.Path, "/ssh"):
			w.Write([]byte(`{"host":"10.0.0.2","port":22,"user":"root","bastion":{"host":"jump.test","port":2222,"user":"j"},"host_key_fingerprint":"SHA256:x","ready":true}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":"not_found"}}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func tokenFile(t *testing.T, mode os.FileMode) string {
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(secret+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

func run(t *testing.T, env map[string]string, args ...string) (string, string, error) {
	var out, errw bytes.Buffer
	err := realMain(args, &out, &errw, func(k string) string { return env[k] })
	return out.String(), errw.String(), err
}

func TestJWTModeCreateWaitsAndSendsNoActingHeader(t *testing.T) {
	f := newFakeAPI(t)
	tf := tokenFile(t, 0o600)
	out, errs, err := run(t, nil, "-url", f.srv.URL, "-token-file", tf, "create", "-image", "base", "-name", "n", "-key", "k1")
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var got struct {
		Accepted  struct{ OperationID, VMID string }
		Operation struct{ Status string }
	}
	_ = json.Unmarshal([]byte(strings.NewReplacer(`"operation_id"`, `"OperationID"`, `"vm_id"`, `"VMID"`, `"status"`, `"Status"`).Replace(out)), &got)
	if got.Accepted.VMID != "vm1" || got.Operation.Status != "succeeded" {
		t.Fatalf("%s", out)
	}
	for i, a := range f.acting {
		if a != "" {
			t.Fatalf("request %d sent X-Acting-User-ID in jwt mode", i)
		}
	}
	if f.idemp[0] != "k1" {
		t.Fatalf("idempotency key %q", f.idemp[0])
	}
	if strings.Contains(out+errs, secret) {
		t.Fatal("token echoed")
	}
}

func TestEnvTokenAndServiceMode(t *testing.T) {
	f := newFakeAPI(t)
	out, _, err := run(t, map[string]string{"VMCTL_TOKEN": secret, "VMCTL_URL": f.srv.URL}, "-auth", "service", "-user-id", "42", "list")
	if err != nil || !strings.Contains(out, `"vm1"`) {
		t.Fatalf("%v %s", err, out)
	}
	if f.acting[0] != "42" {
		t.Fatalf("service mode acting header %q", f.acting[0])
	}
	if _, _, err = run(t, map[string]string{"VMCTL_TOKEN": secret}, "-url", f.srv.URL, "-auth", "service", "list"); err == nil {
		t.Fatal("service mode without -user-id accepted")
	}
}

func TestTokenFileSafety(t *testing.T) {
	f := newFakeAPI(t)
	for name, p := range map[string]string{"group/other readable": tokenFile(t, 0o644), "missing": "/nonexistent/vmctl-token", "directory": t.TempDir()} {
		_, errs, err := run(t, nil, "-url", f.srv.URL, "-token-file", p, "list")
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error()+errs, secret) {
			t.Errorf("%s: token in error", name)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatalf("a request was sent with an unsafe credential: %v", f.reqs)
	}
	for name, env := range map[string]map[string]string{"no credential": {}, "whitespace token": {"VMCTL_TOKEN": "a b"}} {
		if _, _, err := run(t, env, "-url", f.srv.URL, "list"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestNoTokenFlagExists(t *testing.T) {
	f := newFakeAPI(t)
	if _, _, err := run(t, nil, "-url", f.srv.URL, "-token", secret, "list"); err == nil {
		t.Fatal("a command-line token flag must not exist (it would leak via argv)")
	}
}

func TestPlaintextOnlyOnLoopback(t *testing.T) {
	if _, _, err := run(t, map[string]string{"VMCTL_TOKEN": secret}, "-url", "http://api.example.com:8090", "list"); err == nil {
		t.Fatal("plaintext non-loopback API accepted")
	}
}

func TestAuthFailureDoesNotLeakToken(t *testing.T) {
	f := newFakeAPI(t)
	out, errs, err := run(t, map[string]string{"VMCTL_TOKEN": "wrong-token"}, "-url", f.srv.URL, "list")
	if err == nil || strings.Contains(out+errs+err.Error(), "wrong-token") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("%v %q %q", err, out, errs)
	}
}

func TestFailedOperationExitsNonzeroAndNoWait(t *testing.T) {
	f := newFakeAPI(t)
	f.opState = "failed"
	env := map[string]string{"VMCTL_TOKEN": secret}
	out, _, err := run(t, env, "-url", f.srv.URL, "delete", "vm1")
	if err == nil || !strings.Contains(out, "failed") {
		t.Fatalf("failed operation must fail: %v %s", err, out)
	}
	f.polls = 0
	out, _, err = run(t, env, "-url", f.srv.URL, "stop", "-no-wait", "vm1")
	if err != nil || f.polls != 0 || strings.Contains(out, `"operation":`) {
		t.Fatalf("no-wait polled: %v %d %s", err, f.polls, out)
	}
}

func TestCommitAndSSHFormats(t *testing.T) {
	f := newFakeAPI(t)
	env := map[string]string{"VMCTL_TOKEN": secret}
	out, _, err := run(t, env, "-url", f.srv.URL, "commit", "vm1", "golden")
	if err != nil || !strings.Contains(out, `"image_id": "img1"`) {
		t.Fatalf("%v %s", err, out)
	}
	out, _, err = run(t, env, "-url", f.srv.URL, "ssh-info", "-format", "ssh", "-known-hosts", "/tmp/kh a", "-key", "/home/me/id", "vm1")
	if err != nil || !strings.HasPrefix(out, "-J j@jump.test:2222 ") || !strings.HasSuffix(strings.TrimSpace(out), "root@10.0.0.2") || !strings.Contains(out, "'UserKnownHostsFile=/tmp/kh a'") {
		t.Fatalf("%v %q", err, out)
	}
	out, _, err = run(t, env, "-url", f.srv.URL, "ssh-info", "-format", "ansible", "vm1")
	if err != nil || !strings.Contains(out, "ansible_host=10.0.0.2") || !strings.Contains(out, "ProxyJump=j@jump.test:2222") {
		t.Fatalf("%v %q", err, out)
	}
	if out, _, err = run(t, env, "-url", f.srv.URL, "ssh-info", "vm1"); err != nil || strings.Contains(strings.ToLower(out), "private") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestResourceOverflowRejectedBeforeRequest(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("uint flag already rejects overflow on 32-bit platforms")
	}
	f := newFakeAPI(t)
	for _, field := range []string{"-vcpu", "-memory-mib"} {
		args := []string{"-url", f.srv.URL, "create", "-image", "base", "-vcpu", "1", "-memory-mib", "256", field, "4294967297", "-no-wait"}
		if _, _, err := run(t, map[string]string{"VMCTL_TOKEN": secret}, args...); err == nil {
			t.Errorf("%s silently truncated to uint32", field)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatalf("overflow reached API: %v", f.reqs)
	}
}

func TestUntrustedHTTPSRejected(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted TLS connection reached handler")
	}))
	defer srv.Close()
	if _, _, err := run(t, map[string]string{"VMCTL_TOKEN": secret}, "-url", srv.URL, "list"); err == nil {
		t.Fatal("untrusted HTTPS server accepted")
	}
}

func TestUsageErrors(t *testing.T) {
	f := newFakeAPI(t)
	env := map[string]string{"VMCTL_TOKEN": secret}
	for _, args := range [][]string{{}, {"nope"}, {"stop"}, {"commit", "a"}, {"create"}, {"create", "-image", "b", "-vcpu", "1"}, {"delete", "a", "b"}, {"get"}} {
		if _, _, err := run(t, env, append([]string{"-url", f.srv.URL}, args...)...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
