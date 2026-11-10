package remote

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerLifecycle(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	m, err := NewManager(t.TempDir(), func(ctx context.Context, p string, w io.Writer) error {
		close(entered)
		fmt.Fprint(w, strings.Repeat("x", 100000))
		select {
		case <-release:
			return os.WriteFile(p, []byte("image"), 0600)
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	s := httptest.NewServer(m)
	defer s.Close()
	c := &Client{Endpoint: s.URL, HTTP: s.Client()}
	j, err := c.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err = c.Start(context.Background()); err == nil {
		t.Fatal("accepted concurrent job")
	}
	if _, err = c.Get(context.Background(), "../bad"); err == nil {
		t.Fatal("accepted traversal")
	}
	for _, body := range []string{`{"socket":"evil"}`, `{} {}`, `null`, strings.Repeat(" ", 5000) + `{}`} {
		r, _ := http.Post(s.URL+"/v1/builds", "application/json", strings.NewReader(body))
		if r.StatusCode != 400 {
			t.Fatalf("body accepted: %d", r.StatusCode)
		}
		r.Body.Close()
	}
	r, _ := http.Get(s.URL + "/v1/builds/" + j.ID + "/image")
	if r.StatusCode != 409 {
		t.Fatal(r.Status)
	}
	r.Body.Close()
	close(release)
	j = waitJob(t, c, j.ID)
	if j.State != "succeeded" || j.Size != 5 || j.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte("image"))) || len(j.Log) > 65536 {
		t.Fatalf("bad job: %+v", j)
	}
	p := filepath.Join(t.TempDir(), "disk")
	if err := c.Download(context.Background(), j, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Download(context.Background(), j, p); err == nil {
		t.Fatal("overwrote output")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "image" {
		t.Fatal(string(b))
	}
}
func waitJob(t *testing.T, c *Client, id string) Job {
	t.Helper()
	for i := 0; i < 200; i++ {
		j, e := c.Get(context.Background(), id)
		if e != nil {
			t.Fatal(e)
		}
		if j.State != "running" && j.State != "cancelling" {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job stuck")
	return Job{}
}
func TestCancellationWaitsForRunner(t *testing.T) {
	entered := make(chan struct{})
	cleanup := make(chan struct{})
	m, _ := NewManager(t.TempDir(), func(ctx context.Context, p string, w io.Writer) error {
		close(entered)
		<-ctx.Done()
		<-cleanup
		return ctx.Err()
	})
	defer m.Close()
	s := httptest.NewServer(m)
	defer s.Close()
	c := &Client{Endpoint: s.URL, HTTP: s.Client()}
	j, _ := c.Start(context.Background())
	<-entered
	if err := c.Cancel(context.Background(), j.ID); err != nil {
		t.Fatal(err)
	}
	if _, e := c.Start(context.Background()); e == nil {
		t.Fatal("released active job before cleanup")
	}
	close(cleanup)
	if j = waitJob(t, c, j.ID); j.State != "cancelled" {
		t.Fatal(j)
	}
}
func TestDownloadRejectsCorruptPartial(t *testing.T) {
	for _, body := range []string{"wrong", "im"} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer s.Close()
			c := &Client{Endpoint: s.URL, HTTP: s.Client()}
			dir := t.TempDir()
			p := filepath.Join(dir, "image")
			j := Job{ID: strings.Repeat("a", 32), Size: 5, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("image")))}
			if c.Download(context.Background(), j, p) == nil {
				t.Fatal("accepted corrupt download")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("left partial download")
			}
		})
	}
}
func TestFailedBuildNeverPublishes(t *testing.T) {
	m, _ := NewManager(t.TempDir(), func(ctx context.Context, p string, w io.Writer) error {
		os.WriteFile(p, []byte("dirty"), 0600)
		return fmt.Errorf("no clean receipt")
	})
	defer m.Close()
	s := httptest.NewServer(m)
	defer s.Close()
	c := &Client{Endpoint: s.URL, HTTP: s.Client()}
	j, _ := c.Start(context.Background())
	j = waitJob(t, c, j.ID)
	if j.State != "failed" {
		t.Fatal(j)
	}
	r, err := http.Get(s.URL + "/v1/builds/" + j.ID + "/image")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 409 {
		t.Fatal(r.Status)
	}
}
