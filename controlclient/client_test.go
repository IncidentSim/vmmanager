package controlclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPContract(t *testing.T) {
	var calls []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-secret" || r.Header.Get("X-Acting-User-ID") != "42" {
			t.Error("missing verified principal")
		}
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/vms":
			if r.Method == "GET" {
				w.Write([]byte(`{"vms":[{"id":"vm1","name":"lab","resources":{"vcpu":1,"memory_mib":512}}]}`))
				return
			}
			if r.Header.Get("Idempotency-Key") != "tg:100:create" {
				t.Error("unstable key")
			}
			var body CreateRequest
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Name != "lab" || body.ImageID != "ubuntu" {
				t.Error("wrong request")
			}
			w.WriteHeader(202)
			w.Write([]byte(`{"operation_id":"op1","vm_id":"vm1"}`))
		case "/v1/operations/op1":
			w.Write([]byte(`{"id":"op1","status":"succeeded","kind":"create","vm_id":"vm1"}`))
		case "/v1/vms/a/b":
			w.Write([]byte(`{"id":"a/b"}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	c, err := New(s.URL, "service-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v, err := c.ListVMs(ctx, 42)
	if err != nil || len(v) != 1 || v[0].Resources.MemoryMiB != 512 {
		t.Fatalf("%+v %v", v, err)
	}
	a, err := c.Create(ctx, 42, "tg:100:create", CreateRequest{Name: "lab", ImageID: "ubuntu"})
	if err != nil || a.VMID != "vm1" {
		t.Fatalf("%+v %v", a, err)
	}
	op, err := c.Wait(ctx, 42, a.OperationID, time.Millisecond)
	if err != nil || op.Status != "succeeded" {
		t.Fatalf("%+v %v", op, err)
	}
	_, err = c.VM(ctx, 42, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1] != "GET /v1/vms/a%2Fb" {
		t.Fatal(calls)
	}
}

func TestClientSafety(t *testing.T) {
	for _, u := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/?token=x", "ftp://localhost"} {
		if _, e := New(u, "token", nil); e == nil {
			t.Errorf("accepted %s", u)
		}
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"error", `{"error":{"code":"unavailable","message":"secret path /private and token"}}`, 503},
		{"malformed", `broken secret`, 200},
		{"oversize", strings.Repeat("x", (4<<20)+1), 200},
		{"trailing", `{"vms":[]} {}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer s.Close()
			c, _ := New(s.URL, "token", nil)
			_, e := c.ListVMs(context.Background(), 42)
			if e == nil || strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "/private") {
				t.Fatalf("unsafe error: %v", e)
			}
		})
	}
}
func TestPollingBounded(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"op","status":"running","kind":"create"}`))
	}))
	defer s.Close()
	c, _ := New(s.URL, "token", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	op, e := c.Wait(ctx, 42, "op", time.Millisecond)
	if e == nil || op.Status != "running" {
		t.Fatalf("%+v %v", op, e)
	}
}
func TestRejectRedirectAndInvalidIdentity(t *testing.T) {
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; http.Redirect(w, r, "http://example.com", 302) }))
	defer s.Close()
	c, _ := New(s.URL, "token", nil)
	if _, e := c.ListVMs(context.Background(), 0); e == nil || hits != 0 {
		t.Fatal("invalid identity dispatched")
	}
	if _, e := c.Create(context.Background(), 42, "", CreateRequest{}); e == nil || hits != 0 {
		t.Fatal("missing key dispatched")
	}
	if _, e := c.ListVMs(context.Background(), 42); e == nil || hits != 1 {
		t.Fatal("redirect followed")
	}
}
