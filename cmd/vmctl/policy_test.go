package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/petruekhin/vibe-firecracker/controlclient"
)

func TestCreateProfileTTLFlags(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		if b["profile"] != "small" || b["ttl_seconds"] != float64(60) || b["resources"] != nil || r.Header.Get("Idempotency-Key") != "retry" {
			t.Errorf("request: %v", b)
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"operation_id":"op-test","vm_id":"bot-test"}`))
	}))
	defer s.Close()
	c, err := controlclient.NewUser(s.URL, "test-only", 11, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out, log bytes.Buffer
	err = execute(context.Background(), c, 11, []string{"create", "-image", "ubuntu", "-profile", "small", "-ttl-seconds", "60", "-key", "retry", "-no-wait"}, &out, &log)
	if err != nil {
		t.Fatal(err)
	}
}
