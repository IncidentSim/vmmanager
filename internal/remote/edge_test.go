package remote

import (
	"context"
	"fmt"
	"io"

	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestBoundedLogRemainsDecodableAfterJSONEscaping(t *testing.T) {
	m, e := NewManager(t.TempDir(), func(ctx context.Context, p string, w io.Writer) error {
		fmt.Fprint(w, strings.Repeat("\x00", MaxLog))
		return os.WriteFile(p, []byte("image"), 0600)
	})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	s := httptest.NewServer(m)
	defer s.Close()
	c := &Client{Endpoint: s.URL, HTTP: s.Client()}
	j, e := c.Start(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	j = waitJob(t, c, j.ID)
	if len(j.Log) != MaxLog {
		t.Fatal("bad log length", len(j.Log))
	}
}

func TestTraversalUnknownAndMethodsRejected(t *testing.T) {
	m, _ := NewManager(t.TempDir(), func(context.Context, string, io.Writer) error { return nil })
	defer m.Close()
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/v1/builds/../etc/passwd", 400},
		{"GET", "/v1/builds/%2e%2e/etc/passwd", 400},
		{"GET", "/v1/builds/" + strings.Repeat("a", 32), 404},
		{"POST", "/v1/builds?command=bad", 400},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("%s: %d", tc.path, w.Code)
		}
	}

}
