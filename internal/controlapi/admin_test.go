package controlapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
)

func waitDone(t *testing.T, c *vmbot.Controller, u int64, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		o, e := c.ControlOperation(u, id)
		if e != nil {
			t.Fatal(e)
		}
		if o.Status == "succeeded" {
			return
		}
		if o.Status == "failed" {
			t.Fatalf("seed failed: %+v", o)
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("operation timeout")
}

func TestAdminDeleteAllSnapshotAndAuthorization(t *testing.T) {
	s, c, _ := optSetup(t, Options{})
	// An ordinary allowed user is never implicitly an administrator.
	p, b := rawCall(t, s, "POST", "/v1/admin/delete-all", "one", "preview", `{"chat_id":11}`)
	if p.StatusCode != 403 || strings.Contains(string(b), "owner_user_id") {
		t.Fatalf("ordinary inventory disclosure: %d %s", p.StatusCode, b)
	}
	c.ConfigureAdmins([]int64{11})
	for _, u := range []int64{11, 22} {
		o, e := c.AcceptControl(u, "seed", vmbot.ControlRequest{Kind: "create", ImageID: "ubuntu"})
		if e != nil {
			t.Fatal(e)
		}
		waitDone(t, c, u, o.ID)
	}
	p, b = rawCall(t, s, "POST", "/v1/admin/delete-all", "one", "preview", `{"chat_id":11}`)
	if p.StatusCode != 202 {
		t.Fatalf("preview: %d %s", p.StatusCode, b)
	}
	var batch vmbot.AdminBatch
	if e := json.Unmarshal(b, &batch); e != nil {
		t.Fatal(e)
	}
	if len(batch.VMs) != 2 || batch.Token == "" {
		t.Fatalf("bad snapshot: %s", b)
	}
	later, e := c.AcceptControl(22, "later", vmbot.ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if e != nil {
		t.Fatal(e)
	}
	waitDone(t, c, 22, later.ID)
	// Another principal cannot confirm or read the transaction.
	p, _ = rawCall(t, s, "POST", "/v1/admin/delete-all/"+batch.Token, "two", "confirm", `{"chat_id":22}`)
	if p.StatusCode != 403 {
		t.Fatalf("foreign confirm: %d", p.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, b = rawCall(t, s, "POST", "/v1/admin/delete-all/"+batch.Token, "one", "confirm", `{"chat_id":11}`)
		if p.StatusCode != 202 {
			t.Fatalf("confirm: %d %s", p.StatusCode, b)
		}
		json.Unmarshal(b, &batch)
		if batch.Pending == 0 {
			break
		}
		time.Sleep(time.Millisecond * 5)
	}
	if batch.Completed != 2 || batch.Failed != 0 || batch.Pending != 0 {
		t.Fatalf("not complete: %+v", batch)
	}
	if _, e = c.ControlVM(22, later.VMID); e != nil {
		t.Fatalf("later VM deleted: %v", e)
	}
	p, b = rawCall(t, s, "POST", "/v1/admin/delete-all/"+batch.Token, "one", "replay", `{"chat_id":11}`)
	var again vmbot.AdminBatch
	json.Unmarshal(b, &again)
	if p.StatusCode != 202 || again.Completed != 2 {
		t.Fatalf("replay: %d %s", p.StatusCode, b)
	}
	p, b = rawCall(t, s, "GET", "/v1/images", "one", "", "")
	if p.StatusCode != 200 || !strings.Contains(string(b), "ubuntu") {
		t.Fatal("base image changed")
	}
	p, _ = rawCall(t, s, "GET", "/v1/vms/"+later.VMID, "one", "", "")
	if p.StatusCode != 404 {
		t.Fatalf("ordinary owner fence changed: %d", p.StatusCode)
	}
}
