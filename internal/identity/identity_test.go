package identity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCanonicalUsername(t *testing.T) {
	good := map[string]string{
		"alice_example":               "alice_example",
		"@Alice_Example":              "alice_example",
		"ABCDE":                       "abcde",
		"a1234":                       "a1234",
		"a" + strings.Repeat("b", 31): "a" + strings.Repeat("b", 31),
	}
	for in, want := range good {
		got, err := CanonicalUsername(in)
		if err != nil || got != want {
			t.Errorf("%q => %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "@", "abcd", "1abcde", "_abcde", "ab cde", "abc-de", "@@abcde", "abcdé", "abcde\n", " abcde", "abcde ", "a" + strings.Repeat("b", 32), "abc.de", "ab/cde", "аbcde"} {
		if got, err := CanonicalUsername(in); err == nil {
			t.Errorf("%q must be rejected, got %q", in, got)
		}
	}
}

func TestCanonicalListRejectsDuplicatesCaseInsensitively(t *testing.T) {
	got, err := CanonicalList([]string{"@Alice_Example", "bob_example"})
	if err != nil || len(got) != 2 || got[0] != "alice_example" || got[1] != "bob_example" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][]string{{"alice_example", "@ALICE_EXAMPLE"}, {"alice_example", "bad name"}, {"alice_example", "alice_example"}} {
		if _, err := CanonicalList(bad); err == nil {
			t.Errorf("%v must be rejected", bad)
		}
	}
	if got, err := CanonicalList(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty list is valid: %v %v", got, err)
	}
}

func open(t *testing.T, dir string) *Bindings {
	t.Helper()
	b, err := OpenBindings(filepath.Join(dir, "bindings.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFirstClaimPinsAndNeverReassigns(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir)
	if err := b.Claim("@Alice_Example", 101); err != nil {
		t.Fatal(err)
	}
	if err := b.Claim("alice_example", 101); err != nil {
		t.Fatalf("same owner reclaim: %v", err)
	}
	if err := b.Claim("ALICE_EXAMPLE", 202); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("recycled handle must conflict: %v", err)
	}
	if id, ok := b.Owner("alice_example"); !ok || id != 101 {
		t.Fatalf("owner %d %v", id, ok)
	}
	if n := b.Names(101); len(n) != 1 || n[0] != "alice_example" {
		t.Fatalf("names %v", n)
	}
	if n := b.Names(202); len(n) != 0 {
		t.Fatalf("loser must not gain names %v", n)
	}
	// Restart: a fresh instance sees the durable binding.
	b2 := open(t, dir)
	if err := b2.Claim("alice_example", 202); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("binding must survive restart: %v", err)
	}
	if id, _ := b2.Owner("alice_example"); id != 101 {
		t.Fatal("owner changed")
	}
	if b.Claim("alice_example", 0) == nil || b.Claim("alice_example", -4) == nil || b.Claim("bad name", 1) == nil {
		t.Fatal("invalid claims must fail")
	}
	fi, err := os.Stat(filepath.Join(dir, "bindings.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("binding file must be 0600: %v %v", fi, err)
	}
}

func TestConcurrentFirstClaimHasOneWinnerAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	const n = 24
	var wins atomic.Int32
	var winner atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		// Separate instances emulate separate processes (flock between open files).
		b := open(t, dir)
		id := int64(1000 + i)
		go func() {
			defer wg.Done()
			<-start
			if b.Claim("race_example", id) == nil {
				wins.Add(1)
				winner.Store(id)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("winners: %d", wins.Load())
	}
	if id, ok := open(t, dir).Owner("race_example"); !ok || id != winner.Load() {
		t.Fatalf("durable owner %d != winner %d", id, winner.Load())
	}
}

func TestExternalUpdateIsObservedByLongLivedInstance(t *testing.T) {
	dir := t.TempDir()
	a, c := open(t, dir), open(t, dir)
	if err := a.Claim("shared_example", 7); err != nil {
		t.Fatal(err)
	}
	if err := c.Claim("shared_example", 8); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("second process must see first claim: %v", err)
	}
	if !a.BoundTo(7, []string{"shared_example"}) || a.BoundTo(8, []string{"shared_example"}) || a.BoundTo(7, []string{"other_example"}) {
		t.Fatal("BoundTo must follow bindings and the configured set")
	}
}

// A directory fsync failure after the atomic rename is an AMBIGUOUS outcome: the
// claim must report failure, and the persisted file (which may already hold the
// binding) must win on the next claim, never a different owner.
func TestDirectorySyncFailureFailsClosedAndPersistedFileDecides(t *testing.T) {
	for _, stage := range []string{"open", "sync", "close"} {
		dir := t.TempDir()
		b := open(t, dir)
		old := syncDirHook
		syncDirHook = func(string) error { return errors.New("injected " + stage + " failure") }
		err := b.Claim("fault_example", 101)
		if err == nil {
			syncDirHook = old
			t.Fatalf("%s: failed directory sync must fail the claim", stage)
		}
		// While durability cannot be established NOTHING may be honoured: not a
		// retry by the same ID, not another ID, not reads, not a fresh instance.
		if b.Claim("fault_example", 101) == nil || b.Claim("fault_example", 202) == nil {
			syncDirHook = old
			t.Fatalf("%s: claims must fail closed while the directory sync keeps failing", stage)
		}
		fresh := open(t, dir)
		if _, ok := b.Owner("fault_example"); ok || b.BoundTo(101, []string{"fault_example"}) || len(b.Names(101)) != 0 ||
			fresh.BoundTo(101, []string{"fault_example"}) {
			syncDirHook = old
			t.Fatalf("%s: unsynced pin must not grant authority", stage)
		}
		syncDirHook = old
		// Same instance, no restart: the rename did happen, so the file is truth.
		if err := b.Claim("fault_example", 202); !errors.Is(err, ErrBindingConflict) {
			t.Fatalf("%s: another ID must not win after an ambiguous write: %v", stage, err)
		}
		if err := b.Claim("fault_example", 101); err != nil {
			t.Fatalf("%s: retry by the first claimant must succeed: %v", stage, err)
		}
		if id, ok := open(t, dir).Owner("fault_example"); !ok || id != 101 {
			t.Fatalf("%s: durable owner %d %v", stage, id, ok)
		}
	}
}

// Reviewer regression (independent gate): a reader in another instance must not
// be granted authority from a renamed-but-not-yet-synced file while the writer is
// still inside the directory sync. Readers wait behind the writer's exclusive
// lock (bounded) and fail closed.
func TestPinConcurrentReaderWaitsForDurability(t *testing.T) {
	oldWait := lockWait
	lockWait = 300 * time.Millisecond
	defer func() { lockWait = oldWait }()
	p := filepath.Join(t.TempDir(), "bindings.json")
	b, err := OpenBindings(p)
	if err != nil {
		t.Fatal(err)
	}
	old := syncDirHook
	defer func() { syncDirHook = old }()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	syncDirHook = func(string) error {
		once.Do(func() { close(entered) })
		<-release
		return errors.New("injected directory fsync failure")
	}
	done := make(chan error, 1)
	go func() { done <- b.Claim("review_example", 101) }()
	<-entered
	other, err := OpenBindings(p)
	if err != nil {
		t.Fatal(err)
	}
	granted := other.BoundTo(101, []string{"review_example"})
	if _, ok := other.Owner("review_example"); ok {
		granted = true
	}
	close(release)
	if claimErr := <-done; claimErr == nil {
		t.Fatal("claim unexpectedly succeeded")
	}
	if granted {
		t.Error("independent reader granted authority before writer completed directory fsync")
	}
}

// Reviewer regression: after a directory fsync failure no authority may escape
// via BoundTo, a retried Claim or a fresh instance while the sync keeps failing;
// once the sync works again the same file is honoured (durability established).
func TestPinFailedDirectorySyncNeverGrantsAuthority(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bindings.json")
	b, err := OpenBindings(p)
	if err != nil {
		t.Fatal(err)
	}
	old := syncDirHook
	defer func() { syncDirHook = old }()
	calls := 0
	syncDirHook = func(string) error { calls++; return errors.New("injected persistent directory fsync error") }
	if err := b.Claim("review_example", 101); err == nil {
		t.Fatal("initial claim unexpectedly succeeded")
	}
	if b.BoundTo(101, []string{"review_example"}) {
		t.Error("BoundTo grants authority from rename whose directory fsync failed")
	}
	if err := b.Claim("review_example", 101); err == nil {
		t.Error("retry Claim grants authority despite persistent fsync failure")
	}
	other, err := OpenBindings(p)
	if err != nil {
		t.Fatal(err)
	}
	if other.BoundTo(101, []string{"review_example"}) {
		t.Error("second instance grants authority from unsynced visible file")
	}
	if calls < 3 {
		t.Errorf("every read/retry must re-attempt the directory sync, calls=%d", calls)
	}
	// Recovery: when the sync works, the visible pin is honoured, still pinned.
	syncDirHook = old
	if !other.BoundTo(101, []string{"review_example"}) || b.Claim("review_example", 202) == nil || b.Claim("review_example", 101) != nil {
		t.Error("pin must be honoured once durability is established, and never reassigned")
	}
}

func TestDurabilityIsReestablishedForNewFileVersionsOnly(t *testing.T) {
	dir := t.TempDir()
	b := open(t, dir)
	if err := b.Claim("sync_example", 1); err != nil {
		t.Fatal(err)
	}
	old := syncDirHook
	defer func() { syncDirHook = old }()
	calls := 0
	syncDirHook = func(d string) error { calls++; return old(d) }
	for i := 0; i < 5; i++ {
		if !b.BoundTo(1, []string{"sync_example"}) {
			t.Fatal("durable pin must be honoured")
		}
	}
	if calls != 0 {
		t.Fatalf("an already-synced file version must not be re-synced per read (%d)", calls)
	}
	// Another instance (process) replaces the file: the new version is synced
	// before this instance honours it.
	if err := open(t, dir).Claim("sync_two_example", 2); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if !b.BoundTo(2, []string{"sync_two_example"}) || calls != 1 {
		t.Fatalf("new file version must be synced once before use (calls=%d)", calls)
	}
}

func TestSyncDirReportsRealErrors(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("open failure of the directory must be reported")
	}
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptOrUnsafeBindingFileFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"garbage":    `{`,
		"unknownkey": `{"version":1,"bindings":{},"x":1}`,
		"dup":        `{"version":1,"bindings":{"alice_example":1,"alice_example":2}}`,
		"badname":    `{"version":1,"bindings":{"Alice_Example":1}}`,
		"badid":      `{"version":1,"bindings":{"alice_example":0}}`,
		"version":    `{"version":2,"bindings":{}}`,
		"trailing":   `{"version":1,"bindings":{}} {}`,
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "bindings.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, err := OpenBindings(p); err == nil {
			if cerr := b.Claim("fresh_example", 5); cerr == nil {
				t.Errorf("%s: corrupt file must not accept claims", name)
			}
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "bindings.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"bindings":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := OpenBindings(p); err == nil {
		if b.Claim("fresh_example", 5) == nil {
			t.Error("group/world readable binding file must be refused")
		}
	}
}
