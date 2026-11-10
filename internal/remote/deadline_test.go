package remote

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJobDeadlineCancelsAndKeepsSlotUntilExit(t *testing.T) {
	cleanup := make(chan struct{})
	defer close(cleanup)
	cancelled := make(chan struct{})
	m, _ := NewManager(t.TempDir(), func(ctx context.Context, p string, w io.Writer) error {
		<-ctx.Done()
		close(cancelled)
		<-cleanup
		return ctx.Err()
	})
	m.jobTimeout = 10 * time.Millisecond
	s := httptest.NewServer(m)
	defer s.Close()
	c := &Client{Endpoint: s.URL, HTTP: s.Client()}
	if _, e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("deadline did not cancel")
	}
	if _, e := c.Start(context.Background()); e == nil {
		t.Fatal("deadline freed slot before exit")
	}
}
