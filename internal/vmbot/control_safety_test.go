package vmbot

import (
	"context"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/vmlibtest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControlSnapshotDoesNotAliasResources(t *testing.T) {
	t.Parallel()
	s, e := OpenControlStore(t.TempDir()+"/state", 0)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	svc, _ := vmlib.NewService(vmlibtest.New(2, "ubuntu"))
	c := NewController(svc, s, &controlTerms{}, ControllerConfig{})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	_, e = c.AcceptControl(11, "k", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if e != nil {
		t.Fatal(e)
	}
	// Effective resources are written by the owner outcome, never by a request.
	if e = s.Update(func(st *State) { st.VMs[0].Resources = &ControlResources{VCPU: 1, MemoryMiB: 512} }); e != nil {
		t.Fatal(e)
	}
	s.Snapshot().VMs[0].Resources.MemoryMiB = 9999
	v := s.Snapshot().VMs[0]
	if v.Resources.VCPU != 1 || v.Resources.MemoryMiB != 512 {
		t.Fatal("resource snapshot aliased caller memory")
	}
}

func TestControlMigrationFailsWithoutOwnerBeforeWriting(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	raw := []byte(`{"version":1,"generation":7,"offset":99,"vm":{"id":"bot-abcdef","token":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","phase":"active","generation":7}}`)
	if e := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	if s, e := OpenControlStore(dir, 0); e == nil {
		s.Close()
		t.Fatal("missing owner accepted")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if string(got) != string(raw) {
		t.Fatal("failed migration changed source")
	}
	s, e := OpenControlStore(dir, 11)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	v := s.Snapshot().VMs[0]
	if v.OwnerUserID != 11 || v.Generation != 7 || s.Snapshot().Offset != 99 {
		t.Fatal("lost legacy identity")
	}
	if e = s.Update(func(st *State) { st.VMs[0].OwnerUserID = 22 }); e == nil {
		t.Fatal("owner was mutable")
	}
}

// A v2 multi-VM state (the previous production format) is adopted only for the
// explicitly configured legacy owner; the exact v2 bytes are backed up and the
// identity, capability token, generation, phase, Seq and delivery offset survive.
func TestControlMigrationFromV2PreservesIdentityAndBacksUpExactBytes(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	tok := "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	raw := []byte(`{"version":2,"generation":9,"offset":1234,"vms":[{"id":"bot-0123456789abcdef","token":"` + tok + `","phase":"active","generation":8,"seq":5,"label":"old-lab"}]}`)
	if e := os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	s, e := OpenControlStore(dir, 11)
	if e != nil {
		t.Fatal(e)
	}
	v := s.Snapshot().VMs[0]
	if v.OwnerUserID != 11 || v.ID != "bot-0123456789abcdef" || v.Token != tok || v.Generation != 8 || v.Seq != 5 || v.Phase != PhaseActive || v.Label != "old-lab" || s.Snapshot().Offset != 1234 {
		t.Fatalf("identity not preserved: %+v offset=%d", v, s.Snapshot().Offset)
	}
	found := false
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		if b, e := os.ReadFile(filepath.Join(dir, ent.Name())); e == nil && string(b) == string(raw) && ent.Name() != "state.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("exact v2 bytes were not preserved in a backup")
	}
	s.Close()
	// Restart with a different (or no) legacy owner never reassigns ownership.
	s2, e := OpenControlStore(dir, 22)
	if e != nil {
		t.Fatal(e)
	}
	defer s2.Close()
	if s2.Snapshot().VMs[0].OwnerUserID != 11 {
		t.Fatal("restart reassigned ownership")
	}
}

func TestControlTerminalFenceIncludesRemovedOwner(t *testing.T) {
	t.Parallel()
	st, e := OpenControlStore(filepath.Join(t.TempDir(), "state"), 0)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	svc, _ := vmlib.NewService(vmlibtest.New(3, "ubuntu"))
	terms := &fakeTerms{}
	c := NewController(svc, st, terms, ControllerConfig{Profile: "small"})
	c.ConfigureControl(ControlPolicy{AllowedUsers: []int64{11}, SharedImages: []string{"ubuntu"}, MaxVMs: 2, MaxVMsPerUser: 1})
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	o, e := c.AcceptControl(11, "create", ControlRequest{Kind: "create", ImageID: "ubuntu"})
	if e != nil {
		t.Fatal(e)
	}
	awaitControl(t, c, 11, o.ID)
	r, _ := st.Snapshot().ByID(o.VMID)
	if e = c.ensureTerminalLocked(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	terms.mu.Lock()
	target := terms.started[len(terms.started)-1]
	terms.mu.Unlock()
	c.control.AllowedUsers = []int64{22}
	if target.authorize() {
		t.Fatal("removed owner still authorized")
	}
}
func awaitControl(t *testing.T, c *Controller, u int64, id string) ControlOperation {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		o, e := c.ControlOperation(u, id)
		if e != nil {
			t.Fatal(e)
		}
		if o.Status == "succeeded" || o.Status == "failed" {
			return o
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation timeout")
	return ControlOperation{}
}
