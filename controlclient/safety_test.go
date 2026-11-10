package controlclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRejectMalformedSuccessEnvelope(t *testing.T) {
	for _, tc := range []struct {
		path, body string
		call       func(*Client) error
	}{
		{"/v1/vms", `null`, func(c *Client) error { _, e := c.ListVMs(context.Background(), 42); return e }},
		{"/v1/vms", `{}`, func(c *Client) error { _, e := c.Create(context.Background(), 42, "key", CreateRequest{}); return e }},
		{"/v1/operations/op", `{"id":"other","status":"succeeded"}`, func(c *Client) error { _, e := c.Operation(context.Background(), 42, "op"); return e }},
	} {
		t.Run(tc.body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					w.WriteHeader(202)
				}
				w.Write([]byte(tc.body))
			}))
			defer s.Close()
			c, _ := New(s.URL, "token", nil)
			if tc.call(c) == nil {
				t.Fatal("accepted malformed success envelope")
			}
		})
	}
}
func TestUserCredentialDoesNotDelegate(t *testing.T) {
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("X-Acting-User-ID") != "" {
			t.Error("ordinary user credential illegally delegated")
		}
		w.Write([]byte(`{"vms":[]}`))
	}))
	defer s.Close()
	c, e := NewUser(s.URL, "user-token", 42, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.ListVMs(context.Background(), 42); e != nil {
		t.Fatal(e)
	}
	if _, e = c.ListVMs(context.Background(), 43); e == nil || hits != 1 {
		t.Fatal("changed bound user")
	}
}
func TestAllMutationAndReadWirePaths(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			if r.Header.Get("Idempotency-Key") != "stable" {
				t.Error("missing key")
			}
			w.WriteHeader(202)
			w.Write([]byte(`{"operation_id":"op","vm_id":"vm","image_id":"img"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/images":
			w.Write([]byte(`{"images":[{"id":"img","name":"saved","shared":false}]}`))
		case "/v1/images/img":
			w.Write([]byte(`{"id":"img","name":"saved","shared":false}`))
		case "/v1/capabilities":
			w.Write([]byte(`{"max_vms":2,"max_vms_per_user":1,"profiles":[{"name":"small","vcpu":1,"memory_mib":512}]}`))
		case "/v1/vms/vm/ssh":
			w.Write([]byte(`{"host":"guest","port":22,"user":"root","ready":true,"host_key_fingerprint":"SHA256:x"}`))
		case "/v1/operations/op":
			w.Write([]byte(`{"id":"op","status":"failed","error":{"code":"unavailable","message":"secret"}}`))
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer s.Close()
	c, _ := New(s.URL, "token", nil)
	ctx := context.Background()
	for _, fn := range []func() (Accepted, error){func() (Accepted, error) { return c.Stop(ctx, 42, "vm", "stable") }, func() (Accepted, error) { return c.Delete(ctx, 42, "vm", "stable") }, func() (Accepted, error) { return c.Commit(ctx, 42, "vm", "stable", "saved") }, func() (Accepted, error) { return c.Terminal(ctx, 42, "vm", "stable") }, func() (Accepted, error) { return c.DeleteImage(ctx, 42, "img", "stable") }} {
		a, e := fn()
		if e != nil || a.OperationID != "op" {
			t.Fatalf("%+v %v", a, e)
		}
	}
	if v, e := c.Images(ctx, 42); e != nil || len(v) != 1 {
		t.Fatal(v, e)
	}
	if v, e := c.Image(ctx, 42, "img"); e != nil || v.Name != "saved" {
		t.Fatal(v, e)
	}
	if v, e := c.Capabilities(ctx, 42); e != nil || len(v.Profiles) != 1 {
		t.Fatal(v, e)
	}
	if v, e := c.SSH(ctx, 42, "vm"); e != nil || v.Port != 22 {
		t.Fatal(v, e)
	}
	if v, e := c.Wait(ctx, 42, "op", time.Millisecond); e == nil || v.Status != "failed" {
		t.Fatal(v, e)
	}
}
