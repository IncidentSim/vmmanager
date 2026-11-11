package controlclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublicSSHEndpointWire(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/vms/public-vm/ssh" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":"ssh.example.test","port":22003,"user":"root","ready":true}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "token", nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.SSH(context.Background(), 42, "public-vm")
	if err != nil || info.Host != "ssh.example.test" || info.Port != 22003 || len(info.Bastion) != 0 || !info.Ready {
		t.Fatalf("%+v %v", info, err)
	}
}
