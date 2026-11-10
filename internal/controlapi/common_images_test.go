package controlapi

import (
	"testing"
	"time"
)

func waitOp(t *testing.T, s interface{}, call func() (int, map[string]any)) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		code, o := call()
		if code == 200 && o["status"] == "succeeded" {
			return
		}
		if code == 200 && o["status"] == "failed" {
			t.Fatalf("operation failed: %v", o)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
}

func TestCommonImageHTTPProjection(t *testing.T) {
	s, _ := setup(t)
	wait := func(token, op string) {
		waitOp(t, nil, func() (int, map[string]any) { return call(t, s, "GET", "/v1/operations/"+op, token, "", "", "") })
	}
	code, a := call(t, s, "POST", "/v1/vms", "one", "c1", `{"name":"src","image_id":"ubuntu"}`, "")
	if code != 202 {
		t.Fatal(code, a)
	}
	vm := a["vm_id"].(string)
	wait("one", a["operation_id"].(string))
	code, a = call(t, s, "POST", "/v1/vms/"+vm+"/stop", "one", "c2", `{}`, "")
	if code != 202 {
		t.Fatal(code, a)
	}
	wait("one", a["operation_id"].(string))
	code, a = call(t, s, "POST", "/v1/vms/"+vm+"/commit", "one", "c3", `{"name":"common-one"}`, "")
	if code != 202 {
		t.Fatal(code, a)
	}
	img := a["image_id"].(string)
	wait("one", a["operation_id"].(string))

	find := func(token string) map[string]any {
		code, l := call(t, s, "GET", "/v1/images", token, "", "", "")
		if code != 200 {
			t.Fatal(code, l)
		}
		for _, x := range l["images"].([]any) {
			if m := x.(map[string]any); m["id"] == img {
				return m
			}
		}
		return nil
	}
	creator, other := find("one"), find("two")
	if creator == nil || other == nil {
		t.Fatalf("common image missing: %v %v", creator, other)
	}
	if creator["shared"] != true || creator["phase"] != "ready" || creator["can_delete"] != true {
		t.Fatalf("creator projection %v", creator)
	}
	if other["shared"] != true || other["phase"] != "ready" || other["can_delete"] != false {
		t.Fatalf("other projection %v", other)
	}
	if _, leak := other["owner_user_id"]; leak {
		t.Fatal("creator identity exposed")
	}
	code, got := call(t, s, "GET", "/v1/images/"+img, "two", "", "", "")
	if code != 200 || got["can_delete"] != false {
		t.Fatal(code, got)
	}
	// System image: shared, ready, never deletable.
	code, sys := call(t, s, "GET", "/v1/images/ubuntu", "one", "", "", "")
	if code != 200 || sys["shared"] != true || sys["can_delete"] != false || sys["phase"] != "ready" {
		t.Fatal(code, sys)
	}
	code, _ = call(t, s, "DELETE", "/v1/images/ubuntu", "one", "d0", "", "")
	if code != 409 {
		t.Fatalf("system delete %d", code)
	}
	// Other user can boot, but not see the source VM, and cannot delete.
	code, _ = call(t, s, "GET", "/v1/vms/"+vm, "two", "", "", "")
	if code != 404 {
		t.Fatalf("source VM leaked %d", code)
	}
	code, _ = call(t, s, "DELETE", "/v1/images/"+img, "two", "d1", "", "")
	if code != 403 {
		t.Fatalf("foreign delete %d", code)
	}
	code, b := call(t, s, "POST", "/v1/vms", "two", "b1", `{"name":"mine","image_id":"`+img+`"}`, "")
	if code != 202 {
		t.Fatalf("boot common image %d %v", code, b)
	}
	wait("two", b["operation_id"].(string))
	// Referenced by another owner's VM: permission still true, delete is 409.
	if m := find("one"); m["can_delete"] != true {
		t.Fatalf("can_delete is permission only: %v", m)
	}
	code, _ = call(t, s, "DELETE", "/v1/images/"+img, "one", "d2", "", "")
	if code != 409 {
		t.Fatalf("referenced delete %d", code)
	}
}
