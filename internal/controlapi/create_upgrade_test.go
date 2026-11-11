package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
)

// Frozen pre-profile DTO and algorithm from baseline control.go/http.go. Do not
// use the current ControlRequest or replay helper to construct upgrade fixtures.
type baselineResources struct {
	VCPU      uint32 `json:"vcpu"`
	MemoryMiB uint32 `json:"memory_mib"`
}
type baselineRequest struct {
	Kind         string             `json:"kind"`
	VMID         string             `json:"vm_id,omitempty"`
	ImageID      string             `json:"image_id,omitempty"`
	Name         string             `json:"name,omitempty"`
	Resources    *baselineResources `json:"resources,omitempty"`
	SSHPublicKey string             `json:"ssh_public_key,omitempty"`
}

func TestOldHTTPStateUpgradeReplay(t *testing.T) {
	dir := t.TempDir() + "/state"
	st, err := vmbot.OpenControlStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Drive the old HTTP decoding + semantic digest through real HTTP, and persist
	// settled associations (VMs can already be deleted before the upgrade).
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var dto struct {
			Name         string             `json:"name"`
			ImageID      string             `json:"image_id"`
			Resources    *baselineResources `json:"resources"`
			SSHPublicKey string             `json:"ssh_public_key"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&dto); err != nil {
			http.Error(w, "decode", 422)
			return
		}
		raw, _ := json.Marshal(baselineRequest{Kind: "create", ImageID: dto.ImageID, Name: dto.Name, Resources: dto.Resources, SSHPublicKey: dto.SSHPublicKey})
		sum := sha256.Sum256(raw)
		key := r.Header.Get("Idempotency-Key")
		op := vmbot.ControlOperation{ID: "op-" + key, OwnerUserID: 11, Key: key, Digest: hex.EncodeToString(sum[:]), Kind: "create", VMID: "bot-" + key, ImageID: dto.ImageID, Status: "succeeded"}
		if err := st.Update(func(s *vmbot.State) { s.Operations = append(s.Operations, op) }); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		respond(w, 202, op)
	}))
	bodies := map[string]string{"plain": `{"image_id":"ubuntu"}`, "resources": `{"image_id":"ubuntu","resources":{"vcpu":2,"memory_mib":777}}`, "key": `{"image_id":"ubuntu","ssh_public_key":"` + genKey(t) + ` original-comment"}`}
	for key, body := range bodies {
		r, b := rawCall(t, old, "POST", "/v1/vms", "one", key, body)
		if r.StatusCode != 202 {
			t.Fatalf("baseline: %d %s", r.StatusCode, b)
		}
	}
	old.Close()
	st.Close()
	st, err = vmbot.OpenControlStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	backend := vmlibtest.New(4, "ubuntu")
	svc, _ := vmlib.NewService(backend)
	ctl := vmbot.NewController(svc, st, &terms{}, vmbot.ControllerConfig{Profile: "removed"})
	ctl.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11, 22}, MaxVMs: 4, MaxVMsPerUser: 2})
	if err := ctl.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	h, err := New(ctl, []Credential{{SHA256: digest("one"), UserID: 11}, {SHA256: digest("two"), UserID: 22}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Configure(Options{Policy: Policy{DefaultProfile: "replacement"}}); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	defer s.Close()
	for key, body := range bodies {
		r, b := rawCall(t, s, "POST", "/v1/vms", "one", key, body)
		if r.StatusCode != 202 || !strings.Contains(string(b), `"operation_id":"op-`+key+`"`) {
			t.Fatalf("upgrade %s: %d %s", key, r.StatusCode, b)
		}
	}
	for _, tc := range []struct {
		token, key, body string
		status           int
	}{
		{"one", "resources", `{"image_id":"ubuntu","resources":{"vcpu":2,"memory_mib":778}}`, 409},
		{"one", "resources", `{"image_id":"ubuntu","resources":{"vcpu":2,"memory_mib":777,"cpu_limit_milli":1000}}`, 422},
		{"one", "fresh", bodies["resources"], 422},
		{"two", "resources", bodies["resources"], 422},
		{"bad", "resources", bodies["resources"], 401},
		{"one", "plain", `{"image_id":"other"}`, 409},
		{"one", "plain", `{"image_id":"ubuntu","profile":"replacement"}`, 409},
		{"one", "key", strings.ReplaceAll(bodies["key"], "original-comment", "changed-comment"), 409},
	} {
		t.Run(fmt.Sprintf("%s-%s-%d", tc.token, tc.key, tc.status), func(t *testing.T) {
			r, b := rawCall(t, s, "POST", "/v1/vms", tc.token, tc.key, tc.body)
			if r.StatusCode != tc.status {
				t.Fatalf("%d %s", r.StatusCode, b)
			}
			if tc.status == 422 && !strings.Contains(string(b), `"field":"resources"`) {
				t.Fatalf("missing field errors: %s", b)
			}
		})
	}
	if len(st.Snapshot().Operations) != len(bodies) || len(st.Snapshot().VMs) != 0 {
		t.Fatal("replay admitted effects")
	}
}

func TestOmittedProfileIntentSurvivesDefaultChange(t *testing.T) {
	s, ctl, b := optSetup(t, Options{})
	body := `{"image_id":"ubuntu"}`
	r, first := rawCall(t, s, "POST", "/v1/vms", "one", "omitted", body)
	if r.StatusCode != 202 {
		t.Fatalf("%d %s", r.StatusCode, first)
	}
	// Change both public default and remove the originally selected catalogue item.
	s.Close()
	h, err := New(ctl, []Credential{{SHA256: digest("one"), UserID: 11}, {SHA256: digest("two"), UserID: 22}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Configure(Options{Policy: Policy{DefaultProfile: "replacement"}}); err != nil {
		t.Fatal(err)
	}
	s = httptest.NewServer(h)
	defer s.Close()
	b.SetCapabilities(vmlib.Capabilities{Profiles: []vmlib.Profile{{Name: "replacement", VCPU: 2, MemMiB: 600, DiskMiB: 8192}}})
	r, again := rawCall(t, s, "POST", "/v1/vms", "one", "omitted", body)
	if r.StatusCode != 202 {
		t.Fatalf("replay %d %s", r.StatusCode, again)
	}
	var a, z map[string]any
	json.Unmarshal(first, &a)
	json.Unmarshal(again, &z)
	if a["operation_id"] != z["operation_id"] || a["vm_id"] != z["vm_id"] {
		t.Fatal("association changed")
	}
	v, err := ctl.ControlVM(11, a["vm_id"].(string))
	if err != nil || v.Profile != "small" {
		t.Fatalf("snapshot: %+v %v", v, err)
	}
	r, p := rawCall(t, s, "POST", "/v1/vms", "one", "omitted", `{"image_id":"ubuntu","profile":"small"}`)
	if r.StatusCode != 409 {
		t.Fatalf("changed intent %d %s", r.StatusCode, p)
	}
}
