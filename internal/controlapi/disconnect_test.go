package controlapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCallerDisconnectDoesNotCancelAcceptedIntent(t *testing.T) {
	s, c := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/vms", strings.NewReader(`{"name":"durable","image_id":"ubuntu"}`))
	r.Header.Set("Authorization", "Bearer one")
	r.Header.Set("Idempotency-Key", "disconnected")
	p, e := http.DefaultClient.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	p.Body.Close()
	cancel()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		v := c.ControlVMs(11)
		if len(v) == 1 && v[0].Phase == "active" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("caller budget cancelled durable create")
}
