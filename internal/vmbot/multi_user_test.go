package vmbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestMultiUserControllerIsolation(t *testing.T) {
	e := newEnvMax(t, 3)
	e.ctl.cfg.AllowedUserIDs = []int64{testUser, 5151}
	e.ctl.cfg.MaxVMsPerUser = 1
	a := WithUser(context.Background(), testUser)
	b := WithUser(context.Background(), 5151)
	one, err := e.ctl.Create(a, "same")
	if err != nil {
		t.Fatal(err)
	}
	two, err := e.ctl.Create(b, "same")
	if err != nil {
		t.Fatal(err)
	}
	if one.VMID == two.VMID {
		t.Fatal("shared identity")
	}
	before := e.store.Snapshot()
	calls := 0
	e.lc.get = func(ctx context.Context, id string) (vmlib.VM, error) {
		calls++
		return vmlib.VM{}, errors.New("unexpected backend call")
	}
	for _, ref := range []Ref{{Sel: one.VMID}, {Gen: one.Generation}, {Sel: "bot-absent"}, {Gen: 999}} {
		if _, err := e.ctl.Status(b, ref); err == nil {
			t.Fatal("foreign status accepted")
		}
		if _, err := e.ctl.Terminal(b, ref); err == nil {
			t.Fatal("foreign terminal accepted")
		}
		if _, err := e.ctl.Delete(b, ref); err == nil {
			t.Fatal("foreign delete accepted")
		}
	}
	if calls != 0 || !reflect.DeepEqual(before, e.store.Snapshot()) {
		t.Fatal("foreign access had effects")
	}
	r, err := e.ctl.Resolve(5151, Ref{})
	if err != nil || r.ID != two.VMID {
		t.Fatalf("default not scoped: %+v %v", r, err)
	}
}

func TestMultiUserMigrationAmbiguousBytes(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			token, _ := randToken()
			record := map[string]any{"id": "bot-legacy", "token": token, "phase": PhaseDeleting, "generation": uint64(7), "seq": uint64(9)}
			rawMap := map[string]any{"version": version, "generation": uint64(12), "offset": int64(123)}
			if version == 1 {
				rawMap["vm"] = record
			} else {
				record["label"] = "old"
				rawMap["vms"] = []any{record}
			}
			raw, _ := json.Marshal(rawMap)
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if st, err := OpenStore(dir); err == nil {
				st.Close()
				t.Fatal("ambiguous owner accepted")
			}
			got, _ := os.ReadFile(path)
			if !bytes.Equal(raw, got) {
				t.Fatal("ambiguous migration changed bytes")
			}
			st, err := OpenStore(dir, testUser)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			r := st.Snapshot().VMs[0]
			if r.OwnerUserID != testUser || r.Seq != 9 || r.Generation != 7 || r.Token != token || r.Phase != PhaseDeleting || st.Snapshot().Offset != 123 {
				t.Fatalf("migration lost identity: %+v", r)
			}
			backup, err := os.ReadFile(st.Migration().BackupPath)
			if err != nil || !bytes.Equal(raw, backup) {
				t.Fatal("backup differs")
			}
		})
	}
}

func TestMultiUserConcurrentQuota(t *testing.T) {
	e := newEnvMax(t, 3)
	e.ctl.cfg.AllowedUserIDs = []int64{testUser, 5151}
	e.ctl.cfg.MaxVMsPerUser = 1
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := testUser
			if i%2 == 1 {
				id = 5151
			}
			_, _ = e.ctl.Create(WithUser(context.Background(), id), "")
		}(i)
	}
	wg.Wait()
	s := e.store.Snapshot()
	if len(s.VMs) != 2 {
		t.Fatalf("admitted %d", len(s.VMs))
	}
	counts := map[int64]int{}
	for _, r := range s.VMs {
		counts[r.OwnerUserID]++
	}
	if counts[testUser] != 1 || counts[5151] != 1 {
		t.Fatal(counts)
	}
}

func TestMultiUserZeroCallerDenied(t *testing.T) {
	e := newEnv(t)
	if _, err := e.ctl.Create(context.Background(), "x"); !errors.Is(err, ErrNoVM) {
		t.Fatalf("zero actor: %v", err)
	}
}

func TestMultiUserConfig(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"both":            func(c *Config) { c.AllowedUserIDs = []int64{42} },
		"empty":           func(c *Config) { c.AllowedUserID = 0; c.AllowedUserIDs = []int64{} },
		"duplicate":       func(c *Config) { c.AllowedUserID = 0; c.AllowedUserIDs = []int64{42, 42} },
		"negative":        func(c *Config) { c.AllowedUserID = 0; c.AllowedUserIDs = []int64{-1} },
		"quota":           func(c *Config) { c.MaxVMsPerUser = -1 },
		"legacy conflict": func(c *Config) { c.LegacyOwnerUserID = 43 },
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := writeCfg(t, mut)
			if _, err := LoadConfig(p); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	p, _ := writeCfg(t, func(c *Config) { c.AllowedUserID = 0; c.AllowedUserIDs = []int64{42, 43} })
	c, err := LoadConfig(p)
	if err != nil || c.MaxVMsPerUser != 1 || c.MaxVMs != 2 || c.LegacyOwner() != 0 {
		t.Fatalf("plural config: %+v %v", c, err)
	}
	p, _ = writeCfg(t, nil)
	c, err = LoadConfig(p)
	if err != nil || c.LegacyOwner() != 42 || len(c.UserIDs()) != 1 {
		t.Fatalf("legacy config %+v %v", c, err)
	}
}

func TestMultiUserBotForeignButtonsAndLists(t *testing.T) {
	e := newEnvMax(t, 3)
	e.ctl.cfg.AllowedUserIDs = []int64{testUser, 5151}
	e.ctl.cfg.MaxVMsPerUser = 1
	api := &fakeAPI{}
	b := NewBotUsers([]int64{testUser, 5151}, api, e.ctl, e.store, nil)
	ctx := context.Background()
	for _, id := range []int64{testUser, 5151} {
		b.Handle(ctx, msg(1, id, id, "private", "/create same"))
	}
	if len(e.store.Snapshot().VMs) != 2 {
		t.Fatal(api.texts())
	}
	a, _ := e.ctl.Resolve(testUser, Ref{})
	other, _ := e.ctl.Resolve(5151, Ref{})
	api.sent = nil
	b.Handle(ctx, msg(2, 5151, 5151, "private", "/list"))
	if strings.Contains(api.texts(), a.ID) || !strings.Contains(api.texts(), other.ID) {
		t.Fatal("list leaked foreign VM")
	}
	for _, kind := range []string{"s:", "t:", "d:"} {
		n := len(api.answers)
		before := e.store.Snapshot()
		b.Handle(ctx, cb(3, 5151, 5151, kind+gstr(a.Generation)))
		if len(api.answers) != n+1 || !strings.Contains(api.answers[n], "Устарело") || !reflect.DeepEqual(before, e.store.Snapshot()) {
			t.Fatal("foreign callback accepted")
		}
	}
	api.sent = nil
	for _, u := range []Update{msg(4, 999, 999, "private", "/list"), msg(5, testUser, 5151, "private", "/list"), msg(6, 5151, -1, "group", "/list"), cb(7, testUser, 5151, "t")} {
		b.Handle(ctx, u)
	}
	if len(api.sent) != 0 {
		t.Fatal("spoofed sender/chat served")
	}
	b.Handle(ctx, cb(8, 5151, 5151, "s"))
	if strings.Contains(api.texts(), a.ID) || !strings.Contains(api.texts(), other.ID) {
		t.Fatal("generic callback unscoped")
	}
}

func TestMultiUserOwnerImmutable(t *testing.T) {
	e := newEnv(t)
	_, err := e.ctl.Create(WithUser(context.Background(), testUser), "x")
	if err != nil {
		t.Fatal(err)
	}
	before := e.store.Snapshot()
	if err := e.store.Update(func(s *State) { s.VMs[0].OwnerUserID = 5151 }); err == nil {
		t.Fatal("owner reassigned")
	}
	if !reflect.DeepEqual(before, e.store.Snapshot()) {
		t.Fatal("owner changed")
	}
}

func TestMultiUserRemovedOwnerRestartRetainsOccupancy(t *testing.T) {
	e := newEnvMax(t, 2)
	a := WithUser(context.Background(), testUser)
	_, err := e.ctl.Create(a, "one")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.ctl.Create(a, "two")
	if err != nil {
		t.Fatal(err)
	}
	before := e.store.Snapshot()
	e.restart(t)
	e.ctl.cfg.AllowedUserIDs = []int64{5151}
	e.ctl.cfg.MaxVMsPerUser = 1
	if err := e.ctl.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, e.store.Snapshot()) {
		t.Fatal("removed resources changed")
	}
	if len(e.terms.started) != 0 {
		t.Fatal("terminal started for removed owner")
	}
	if _, err := e.ctl.Terminal(a, Ref{Sel: "one"}); !errors.Is(err, ErrNoVM) {
		t.Fatal(err)
	}
	if _, err := e.ctl.List(a); !errors.Is(err, ErrNoVM) {
		t.Fatal(err)
	}
	if _, err := e.ctl.Create(WithUser(context.Background(), 5151), "new"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("occupancy lost: %v", err)
	}
	e.ctl.cfg.AllowedUserIDs = []int64{testUser}
	if _, err := e.ctl.Terminal(a, Ref{Sel: "one"}); err != nil {
		t.Fatal(err)
	}
	if e.store.Snapshot().VMs[0].Token != before.VMs[0].Token {
		t.Fatal("token rotated")
	}
}

func TestMultiUserWorkflowIdentityRejectsForeignActor(t *testing.T) {
	e := newEnv(t)
	s, err := e.ctl.Create(WithUser(context.Background(), testUser), "one")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{opTerminal, opDelete, opCreate, opReconcile} {
		_, err := e.ctl.submit(OpInput{Kind: kind, VMID: s.VMID, OwnerUserID: 5151}, "foreign-"+kind)
		if err == nil {
			t.Fatal("foreign workflow submitted")
		}
	}
}
