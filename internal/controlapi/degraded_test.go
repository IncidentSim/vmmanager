package controlapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDegradedHealthStillServesAuthenticatedStatus(t *testing.T) {
	s, c := setup(t)
	code, body := call(t, s, "GET", "/healthz", "", "", "", "")
	if code != 200 || body["status"] != "ok" {
		t.Fatal(code, body)
	}
	srv, e := New(c, []Credential{{SHA256: digest("one"), UserID: 11}})
	if e != nil {
		t.Fatal(e)
	}
	srv.SetDegraded(true)
	if !srv.Degraded() {
		t.Fatal("flag lost")
	}
	// Health stays unauthenticated and minimal; it must not reveal operations.
	rec := serve(srv, "GET", "/healthz", "")
	if rec.Code != 200 || len(rec.Body.String()) > 64 || rec.Body.String() == "" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if want := `{"status":"degraded"}`; rec.Body.String() != want+"\n" {
		t.Fatalf("%q", rec.Body.String())
	}
	rec = serve(srv, "GET", "/v1/vms", "one")
	if rec.Code != 200 {
		t.Fatalf("degraded API must keep serving: %d", rec.Code)
	}
}
