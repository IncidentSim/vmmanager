package controlclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func TestPerUserClientSendsOnlyThatUsersBearerAndNoActingHeader(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"vms":[]}`))
	}))
	defer srv.Close()
	c, err := NewPerUser(srv.URL, func(u int64) (string, error) { return "tok-for-" + strconv.FormatInt(u, 10), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []int64{5, 6} {
		if _, err = c.ListVMs(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0].Get("Authorization") != "Bearer tok-for-5" || seen[1].Get("Authorization") != "Bearer tok-for-6" {
		t.Fatalf("%v", seen)
	}
	for _, h := range seen {
		if len(h.Values("X-Acting-User-ID")) != 0 {
			t.Fatal("per-user client must never delegate via X-Acting-User-ID")
		}
	}
}

func TestPerUserClientFailsClosedWithoutToken(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	for name, f := range map[string]func(int64) (string, error){
		"error":   func(int64) (string, error) { return "", errors.New("no") },
		"empty":   func(int64) (string, error) { return "", nil },
		"newline": func(int64) (string, error) { return "a\r\nX: y", nil },
	} {
		c, err := NewPerUser(srv.URL, f, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.ListVMs(context.Background(), 5); err == nil {
			t.Errorf("%s: must fail", name)
		}
	}
	if hit {
		t.Fatal("no request may be sent without a valid per-user token")
	}
	if _, err := NewPerUser(srv.URL, nil, nil); err == nil {
		t.Fatal("token source required")
	}
	if _, err := NewPerUser("http://example.com", func(int64) (string, error) { return "x", nil }, nil); err == nil {
		t.Fatal("plaintext non-loopback must be refused")
	}
}
