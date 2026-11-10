package controlclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRejectUnsafePathSegmentsBeforeDispatch(t *testing.T) {
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.Write([]byte(`{"id":"vm"}`)) }))
	defer s.Close()
	c, _ := New(s.URL, "token", nil)
	for _, id := range []string{"", ".", ".."} {
		if _, e := c.VM(context.Background(), 42, id); e == nil {
			t.Errorf("unsafe ID %q accepted", id)
		}
	}
	if hits != 0 {
		t.Fatal("unsafe segment reached server")
	}
}
