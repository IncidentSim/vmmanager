package controlapi

import (
	"testing"
)

func TestAdminJWTSubjectIndependentOfOrdinaryAllowlist(t *testing.T) {
	e := newJWT(t, []string{"ordinary_user"}, []Credential{{SHA256: digest("service"), Service: true}})
	e.ctl.ConfigureAdmins([]int64{99})
	admin := e.token(t, 99, "")
	if code, _ := call(t, e.srv, "POST", "/v1/admin/delete-all", admin, "preview", `{"chat_id":99}`, ""); code != 202 {
		t.Fatalf("independent admin subject: %d", code)
	}
	if code, _ := call(t, e.srv, "GET", "/v1/vms", admin, "", "", ""); code != 403 {
		t.Fatalf("admin is not ordinary owner access: %d", code)
	}
	ordinary := e.token(t, 11, "")
	if code, _ := call(t, e.srv, "POST", "/v1/admin/delete-all", ordinary, "preview", `{"chat_id":11,"admin":true}`, ""); code != 403 {
		t.Fatalf("spoofed role: %d", code)
	}
	if code, _ := call(t, e.srv, "POST", "/v1/admin/delete-all", admin, "preview", `{"chat_id":99}`, "11"); code != 403 {
		t.Fatalf("JWT delegation: %d", code)
	}
	if code, _ := call(t, e.srv, "POST", "/v1/admin/delete-all", "service", "preview", `{"chat_id":99}`, "99"); code != 403 {
		t.Fatalf("service impersonation: %d", code)
	}
	if code, _ := call(t, e.srv, "POST", "/v1/admin/delete-all", admin, "preview", `{"chat_id":11}`, ""); code != 403 {
		t.Fatalf("foreign chat: %d", code)
	}
	if code, _ := call(t, e.srv, "POST", "/v1/admin/delete-all", admin, "preview", `{"chat_id":99,"owner_user_id":11}`, ""); code != 422 {
		t.Fatalf("caller-selected owner: %d", code)
	}
}
