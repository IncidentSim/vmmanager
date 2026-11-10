package vmbot

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreCreatesPrivateDirAndFileAtomically(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Update(func(st *State) { st.Generation = 7; st.Offset = 42 }); err != nil {
		t.Fatal(err)
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", di.Mode().Perm())
	}
	fi, err := os.Stat(filepath.Join(dir, "state.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", fi, err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "state.json" && e.Name() != "lock" {
			t.Fatalf("leftover file %s", e.Name())
		}
	}
}

func TestStoreExclusiveLockAndReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Update(func(st *State) {
		st.Generation = 3
		st.VM = &VMRecord{ID: "bot-a", Token: "tok", Phase: PhaseActive, Generation: 3}
	})
	if _, err := OpenStore(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open: want ErrLocked got %v", err)
	}
	s.Close()
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got := s2.Snapshot()
	if got.Generation != 3 || got.VM == nil || got.VM.ID != "bot-a" || got.VM.Token != "tok" {
		t.Fatalf("reload: %+v", got)
	}
}

func TestStoreFailsClosedOnBadFiles(t *testing.T) {
	for name, prep := range map[string]func(dir string){
		"corrupt": func(d string) { os.WriteFile(filepath.Join(d, "state.json"), []byte("{not json"), 0o600) },
		"loose":   func(d string) { os.WriteFile(filepath.Join(d, "state.json"), []byte(`{"version":1}`), 0o644) },
		"future":  func(d string) { os.WriteFile(filepath.Join(d, "state.json"), []byte(`{"version":99}`), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "state")
			os.MkdirAll(dir, 0o700)
			prep(dir)
			if s, err := OpenStore(dir); err == nil {
				s.Close()
				t.Fatal("expected error")
			}
		})
	}
	loose := t.TempDir()
	os.Chmod(loose, 0o755)
	if s, err := OpenStore(loose); err == nil {
		s.Close()
		t.Fatal("group-readable state dir must be refused")
	}
}

func TestStoreUpdateRollsBackWhenPersistFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, _ := OpenStore(dir)
	defer s.Close()
	_ = s.Update(func(st *State) { st.Generation = 1 })
	s.dir = filepath.Join(dir, "missing") // force write failure
	if err := s.Update(func(st *State) { st.Generation = 2 }); err == nil {
		t.Fatal("expected persist error")
	}
	if s.Snapshot().Generation != 1 {
		t.Fatal("in-memory state must not advance when persistence fails")
	}
}
