package vmbot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTok returns a syntactically valid, deterministic synthetic capability.
func testTok(n int) string { return fmt.Sprintf("%043d", n) }

// v1File builds a synthetic v1 state file (no real populated state is ever read).
func v1File(gen uint64, offset int64, vm *VMRecord) []byte {
	b, _ := json.Marshal(stateV1{Version: 1, Generation: gen, VM: vm, Offset: offset})
	return b
}

func writeState(t *testing.T, dir string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), b, mode); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateV1PreservesEveryPhaseOffsetAndCapability(t *testing.T) {
	for _, phase := range []string{PhaseCreating, PhaseActive, PhaseDeleting} {
		t.Run(phase, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			rec := VMRecord{ID: "bot-c5d5cd1e8a7894eb", Token: testTok(77), Phase: phase, Generation: 5}
			raw := v1File(9, 123456789, &rec)
			writeState(t, dir, raw, 0o600)

			s, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			check := func(s *Store) {
				t.Helper()
				got := s.Snapshot()
				if got.Version != 2 || got.Generation != 9 || got.Offset != 123456789 || len(got.VMs) != 1 {
					t.Fatalf("migrated state %+v", got)
				}
				v := got.VMs[0]
				if v.ID != rec.ID || v.Token != rec.Token || v.Phase != phase || v.Generation != 5 || v.Label != "vm-5" {
					t.Fatalf("identity/capability/generation/phase must survive: %+v", v)
				}
			}
			check(s)
			m := s.Migration()
			if m == nil {
				t.Fatal("migration must be reported")
			}
			if fi, err := os.Stat(m.BackupPath); err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("backup must be 0600: %v %v", fi, err)
			}
			if b, _ := os.ReadFile(m.BackupPath); !bytes.Equal(b, raw) {
				t.Fatal("backup must hold the exact v1 bytes")
			}
			if fi, _ := os.Stat(filepath.Join(dir, "state.json")); fi.Mode().Perm() != 0o600 {
				t.Fatalf("state mode %o", fi.Mode().Perm())
			}
			s.Close()

			// restart: loads v2 directly, no second migration, nothing changed
			s2, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			if s2.Migration() != nil {
				t.Fatal("v2 must not migrate again")
			}
			check(s2)
		})
	}
}

func TestMigrateV1WithoutVMKeepsGenerationAndOffset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	writeState(t, dir, v1File(4, 8, nil), 0o600)
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if g := s.Snapshot(); g.Generation != 4 || g.Offset != 8 || len(g.VMs) != 0 {
		t.Fatalf("%+v", g)
	}
	// a new VM continues after the high-water mark: generations are never reused
	if err := s.Update(func(st *State) {
		st.Generation++
		st.VMs = append(st.VMs, VMRecord{ID: "bot-new", Token: testTok(2), Phase: PhaseCreating, Generation: st.Generation})
	}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().VMs[0].Generation != 5 {
		t.Fatal("generation must continue monotonically")
	}
}

func TestMigrationBackupIsNeverOverwrittenOnRetry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	rec := VMRecord{ID: "bot-old", Token: testTok(1), Phase: PhaseActive, Generation: 1}
	first := v1File(1, 10, &rec)
	writeState(t, dir, first, 0o600)
	bak := filepath.Join(dir, "state.json.v1.bak")

	// Retry with an identical v1 (crash between backup and rename): the existing
	// backup is reused, not rewritten or duplicated.
	if err := os.WriteFile(bak, first, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Migration().BackupPath != bak {
		t.Fatalf("identical earlier backup must be reused: %s", s.Migration().BackupPath)
	}
	s.Close()

	// A different v1 later (operator rolled back, v1 advanced) must NOT replace the
	// earlier preserved backup.
	rec2 := rec
	rec2.Generation = 2
	second := v1File(2, 99, &rec2)
	writeState(t, dir, second, 0o600)
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if b, _ := os.ReadFile(bak); !bytes.Equal(b, first) {
		t.Fatal("previously preserved backup was overwritten")
	}
	nb := s.Migration().BackupPath
	if nb == bak {
		t.Fatal("a different v1 needs its own backup file")
	}
	if b, _ := os.ReadFile(nb); !bytes.Equal(b, second) {
		t.Fatal("second backup content")
	}
	if fi, _ := os.Stat(nb); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
}

func TestMigrationFailureLeavesV1Untouched(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	rec := VMRecord{ID: "bot-x", Token: testTok(1), Phase: PhaseActive, Generation: 1}
	raw := v1File(1, 5, &rec)
	writeState(t, dir, raw, 0o600)
	// A directory squats the backup name AND all numbered ones' first slot is
	// not needed: make backup creation fail by removing write permission.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if s, err := OpenStore(dir); err == nil {
		s.Close()
		t.Fatal("migration must fail closed when no backup can be written")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "state.json")); !bytes.Equal(b, raw) {
		t.Fatal("v1 state must be untouched after a failed migration")
	}
}

func TestStoreRejectsInvalidStatesBeforeUse(t *testing.T) {
	good := func() State {
		return State{Version: 2, Generation: 3, Offset: 1, VMs: []VMRecord{
			{ID: "bot-a", Token: testTok(1), Phase: PhaseActive, Generation: 1, Label: "one"},
			{ID: "bot-b", Token: testTok(2), Phase: PhaseCreating, Generation: 3, Label: "two"},
		}}
	}
	cases := map[string]func(s *State){
		"duplicate id":         func(s *State) { s.VMs[1].ID = "bot-a" },
		"duplicate token":      func(s *State) { s.VMs[1].Token = s.VMs[0].Token },
		"duplicate generation": func(s *State) { s.VMs[1].Generation = 1 },
		"descending":           func(s *State) { s.VMs[0].Generation, s.VMs[1].Generation = 3, 1 },
		"duplicate label":      func(s *State) { s.VMs[1].Label = "one" },
		"bad label":            func(s *State) { s.VMs[0].Label = "../etc" },
		"bad label prefix":     func(s *State) { s.VMs[0].Label = "bot-1234" },
		"bad phase":            func(s *State) { s.VMs[0].Phase = "zombie" },
		"bad id":               func(s *State) { s.VMs[0].ID = "bad id/../" },
		"short token":          func(s *State) { s.VMs[0].Token = "short" },
		"zero generation":      func(s *State) { s.VMs[0].Generation = 0 },
		"above high-water":     func(s *State) { s.Generation = 2 },
		"negative offset":      func(s *State) { s.Offset = -1 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			st := good()
			mut(&st)
			b, _ := json.Marshal(st)
			writeState(t, dir, b, 0o600)
			if s, err := OpenStore(dir); err == nil {
				s.Close()
				t.Fatal("invalid state must fail closed")
			}
			if b2, _ := os.ReadFile(filepath.Join(dir, "state.json")); !bytes.Equal(b, b2) {
				t.Fatal("a rejected state file must not be modified")
			}
		})
	}
	// control: the valid fixture loads
	dir := filepath.Join(t.TempDir(), "state")
	b, _ := json.Marshal(good())
	writeState(t, dir, b, 0o600)
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	s.Close()
}

func TestStoreRejectsInvalidV1AndUnknownFields(t *testing.T) {
	rec := VMRecord{ID: "bot-a", Token: testTok(1), Phase: PhaseActive, Generation: 2}
	for name, raw := range map[string][]byte{
		"v1 bad phase":       v1File(2, 0, &VMRecord{ID: "bot-a", Token: testTok(1), Phase: "x", Generation: 2}),
		"v1 gen above mark":  v1File(1, 0, &rec),
		"v1 negative offset": v1File(2, -5, &rec),
		"v1 bad token":       v1File(2, 0, &VMRecord{ID: "bot-a", Token: "t", Phase: PhaseActive, Generation: 2}),
		"v1 unknown field":   []byte(`{"version":1,"generation":1,"offset":0,"extra":true}`),
		"v1 trailing data":   append(v1File(2, 0, &rec), []byte(` {"x":1}`)...),
		"v2 unknown field":   []byte(`{"version":2,"generation":1,"offset":0,"vms":[],"extra":1}`),
		"future version":     []byte(`{"version":3,"generation":1}`),
		"zero version":       []byte(`{"generation":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			writeState(t, dir, raw, 0o600)
			if s, err := OpenStore(dir); err == nil {
				s.Close()
				t.Fatal("must fail closed")
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "state.json")); !bytes.Equal(b, raw) {
				t.Fatal("rejected file modified")
			}
			if ents, _ := os.ReadDir(dir); len(ents) > 2 { // state.json + lock only
				for _, e := range ents {
					if strings.Contains(e.Name(), "bak") {
						t.Fatalf("no backup may be made for a rejected file: %s", e.Name())
					}
				}
			}
		})
	}
}

func TestUpdateValidatesBeforeMutatingState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, _ := OpenStore(dir)
	defer s.Close()
	add := func(id string, g uint64, label string) error {
		return s.Update(func(st *State) {
			if g > st.Generation {
				st.Generation = g
			}
			st.VMs = append(st.VMs, VMRecord{ID: id, Token: testTok(int(g)), Phase: PhaseActive, Generation: g, Label: label})
		})
	}
	if err := add("bot-a", 1, "one"); err != nil {
		t.Fatal(err)
	}
	if err := add("bot-b", 2, "one"); err == nil { // duplicate label
		t.Fatal("duplicate label must be refused")
	}
	if err := add("bot-a", 3, "three"); err == nil { // duplicate ID
		t.Fatal("duplicate id must be refused")
	}
	if got := s.Snapshot(); len(got.VMs) != 1 || got.Generation != 1 {
		t.Fatalf("refused updates must not change memory: %+v", got)
	}
	s.Close()
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if len(s2.Snapshot().VMs) != 1 {
		t.Fatal("refused updates must not reach disk")
	}
}

func TestStoreKeepsDeterministicGenerationOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, _ := OpenStore(dir)
	defer s.Close()
	_ = s.Update(func(st *State) {
		st.Generation = 3
		st.VMs = []VMRecord{
			{ID: "bot-c", Token: testTok(3), Phase: PhaseActive, Generation: 3},
			{ID: "bot-a", Token: testTok(1), Phase: PhaseActive, Generation: 1},
		}
	})
	got := s.Snapshot().VMs
	if got[0].Generation != 1 || got[1].Generation != 3 {
		t.Fatalf("order %+v", got)
	}
	for _, sel := range []string{"bot-a", "bot-c"} {
		if _, ok := s.Snapshot().BySelector(sel); !ok {
			t.Fatalf("selector %s", sel)
		}
	}
	if _, ok := s.Snapshot().BySelector(""); ok {
		t.Fatal("empty selector never matches")
	}
}

func TestValidateLabel(t *testing.T) {
	for _, ok := range []string{"a", "dev", "my-vm-2", "x1", strings.Repeat("a", 20)} {
		if ValidateLabel(ok) != nil {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "1abc", "-a", "a-", "A", "a b", "a/b", "a..b", "a;rm", "$(id)", "a`b`", "ä", "bot-1234", strings.Repeat("a", 21),
		"a\nb", "a\x00b", "--help", "../x", "a_b"} {
		if ValidateLabel(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
