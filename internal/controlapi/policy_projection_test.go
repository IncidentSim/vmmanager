package controlapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func TestExpiredErrorAndFractionalCPUProjection(t *testing.T) {
	h := &Server{}
	w := httptest.NewRecorder()
	h.fail(w, vmbot.ErrExpired)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"vm_expired"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	raw, err := json.Marshal(projectVM(vmbot.ControlVMView{Resources: &vmbot.ControlResources{VCPU: 1, MemoryMiB: 500, CPULimitMilli: 500}}))
	if err != nil || !strings.Contains(string(raw), `"cpu_limit":0.5`) || strings.Contains(string(raw), "cpu_limit_milli") {
		t.Fatalf("%s %v", raw, err)
	}
}
func TestAccessAdapterAuthorizesBeforeCredentialRead(t *testing.T) {
	_, ctl, _ := optSetup(t, Options{})
	// Nil Credentials would panic if either store method were touched first.
	a := AccessKeys{Controller: ctl}
	if _, err := a.GeneratedKey(context.Background(), 22, "bot-missing"); err != vmbot.ErrNoVM {
		t.Fatal(err)
	}
}
func TestNonCreatePolicyInputsRejected(t *testing.T) {
	_, ctl, _ := optSetup(t, Options{})
	for _, req := range []vmbot.ControlRequest{
		{Kind: "stop", VMID: "bot-missing", TTLSeconds: 1},
		{Kind: "delete", VMID: "bot-missing", Profile: "small"},
		{Kind: "delete_image", ImageID: "ubuntu", TTLSeconds: 1},
	} {
		if _, err := ctl.AcceptControl(11, "invalid-policy", req); err != vmlib.ErrInvalidArgument {
			t.Fatalf("%+v: %v", req, err)
		}
	}
}
func TestConfigTTLDurationOverflowRefused(t *testing.T) {
	c := Config{MaxTTLSeconds: 1<<63 - 1}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "whole-second") {
		t.Fatal(err)
	}
}
