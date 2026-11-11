package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

func capsServer(t *testing.T, b *vmlibtest.Backend) *httptest.Server {
	t.Helper()
	st, e := vmbot.OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	svc, e := vmlib.NewService(b)
	if e != nil {
		t.Fatal(e)
	}
	c := vmbot.NewController(svc, st, &terms{}, vmbot.ControllerConfig{Profile: "small", PublicOrigin: "https://terminal.test", OpTimeout: time.Second})
	c.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	h, e := New(c, []Credential{{SHA256: digest("one"), UserID: 11}})
	if e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); c.Close(); st.Close() })
	return srv
}

func TestCapabilitiesHTTPReflectsBackendProfilesAndCaps(t *testing.T) {
	b := vmlibtest.New(9, "ubuntu")
	b.SetCapabilities(vmlib.Capabilities{
		MaxVMs:         9, // backend count must NOT replace the API's owner/global quotas
		Profiles:       []vmlib.Profile{{Name: "tiny", VCPU: 1, MemMiB: 256, DiskMiB: 1024}, {Name: "wide", VCPU: 4, MemMiB: 3072, DiskMiB: 8192}},
		MaxTotalMemMiB: 6144,
		MaxTotalVCPU:   6,
		Bastion:        &vmlib.Bastion{Host: "secret-bastion.example.test", Port: 22, User: "jump"},
		SSHUser:        "root",
	})
	s := capsServer(t, b)
	code, body := call(t, s, "GET", "/v1/capabilities", "one", "", "", "")
	if code != 200 {
		t.Fatalf("%d %v", code, body)
	}
	if body["max_vms"] != float64(2) || body["max_vms_per_user"] != float64(1) {
		t.Fatalf("API owner/global quotas lost: %v", body)
	}
	if body["max_total_memory_mib"] != float64(6144) || body["max_total_vcpu"] != float64(6) {
		t.Fatalf("aggregate caps: %v", body)
	}
	if body["backend_max_vms"] != float64(9) {
		t.Fatalf("backend capacity must be reported separately: %v", body)
	}
	wantProfiles := []any{
		map[string]any{"name": "tiny", "cpu_limit": float64(1), "vcpu": float64(1), "memory_mib": float64(256), "disk_mib": float64(1024)},
		map[string]any{"name": "wide", "cpu_limit": float64(4), "vcpu": float64(4), "memory_mib": float64(3072), "disk_mib": float64(8192)},
	}
	if !reflect.DeepEqual(body["profiles"], wantProfiles) {
		t.Fatalf("profiles: %#v", body["profiles"])
	}
	raw, _ := json.Marshal(body)
	if string(raw) == "" || containsAny(string(raw), "secret-bastion", "jump", "root") {
		t.Fatalf("SSH/bastion metadata leaked: %s", raw)
	}

	// Policy change is reflected on the next request: no hardcoded catalog/cache.
	b.SetCapabilities(vmlib.Capabilities{MaxVMs: 2, Profiles: []vmlib.Profile{{Name: "solo", VCPU: 2, MemMiB: 700, DiskMiB: 512}}})
	_, body = call(t, s, "GET", "/v1/capabilities", "one", "", "", "")
	ps, _ := body["profiles"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["name"] != "solo" || ps[0].(map[string]any)["memory_mib"] != float64(700) {
		t.Fatalf("stale profiles: %v", body)
	}
	if _, present := body["max_total_memory_mib"]; present {
		t.Fatalf("unenforced aggregate cap must be omitted, not 0: %v", body)
	}
}

func TestCapabilitiesHTTPEmptyProfilesIsArrayAndAuthRequired(t *testing.T) {
	b := vmlibtest.New(1, "ubuntu")
	b.SetCapabilities(vmlib.Capabilities{MaxVMs: 1})
	s := capsServer(t, b)
	if code, _ := call(t, s, "GET", "/v1/capabilities", "", "", "", ""); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}
	code, body := call(t, s, "GET", "/v1/capabilities", "one", "", "", "")
	if ps, ok := body["profiles"].([]any); code != 200 || !ok || len(ps) != 0 {
		t.Fatalf("empty profiles must be [], got %d %v", code, body)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, x := range subs {
		for i := 0; i+len(x) <= len(s); i++ {
			if s[i:i+len(x)] == x {
				return true
			}
		}
	}
	return false
}

var _ = http.StatusOK
