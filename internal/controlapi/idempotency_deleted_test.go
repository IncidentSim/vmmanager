package controlapi

import (
	"testing"
	"time"
)

func TestMutationReplaySurvivesVMDeletion(t *testing.T) {
	for _, action := range []string{"stop", "commit", "terminal"} {
		t.Run(action, func(t *testing.T) {
			s, _ := setup(t)
			wait := func(id string) {
				t.Helper()
				for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
					code, b := call(t, s, "GET", "/v1/operations/"+id, "one", "", "", "")
					if code == 200 && b["status"] == "succeeded" {
						return
					}
					if b["status"] == "failed" {
						t.Fatalf("operation failed: %v", b)
					}
				}
				t.Fatal("operation timeout")
			}
			code, created := call(t, s, "POST", "/v1/vms", "one", "create", `{"image_id":"ubuntu"}`, "")
			if code != 202 {
				t.Fatal(code, created)
			}
			wait(created["operation_id"].(string))
			id := created["vm_id"].(string)
			body := `{}`
			if action == "commit" {
				body = `{"name":"derived"}`
			}
			path := "/v1/vms/" + id + "/" + action
			code, original := call(t, s, "POST", path, "one", "original", body, "")
			if code != 202 {
				t.Fatal(code, original)
			}
			wait(original["operation_id"].(string))
			code, deleted := call(t, s, "DELETE", "/v1/vms/"+id, "one", "delete", "", "")
			if code != 202 {
				t.Fatal(code, deleted)
			}
			wait(deleted["operation_id"].(string))
			code, replay := call(t, s, "POST", path, "one", "original", body, "")
			if code != 202 || replay["operation_id"] != original["operation_id"] {
				t.Fatalf("durable replay lost: %d %v", code, replay)
			}
			code, _ = call(t, s, "POST", path, "two", "original", body, "")
			if code != 404 {
				t.Fatalf("other owner gained replay: %d", code)
			}
			code, _ = call(t, s, "POST", path, "one", "new-key", body, "")
			if code != 404 {
				t.Fatalf("new intent on absent VM accepted: %d", code)
			}
			if action == "commit" {
				code, _ = call(t, s, "POST", path, "one", "original", `{"name":"different"}`, "")
				if code != 409 {
					t.Fatalf("changed replay not a conflict: %d", code)
				}
			}
		})
	}
}
