package nextermaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/nextermaccess/nxtest"
)

type fakeAPI struct {
	mu       sync.Mutex
	vms      map[int64]map[string]bool // owner -> live VM IDs
	noKey    map[string]bool
	keyVMID  string // override returned key VMID
	listErr  bool
	getErr   bool
	sshErr   bool
	keyCalls int
	expired  map[string]bool
}

func (a *fakeAPI) view(id string) controlclient.VM {
	v := controlclient.VM{ID: id, Name: "box"}
	if a.expired[id] {
		past := time.Now().Add(-time.Hour)
		v.ExpiresAt = &past
	}
	return v
}

func (a *fakeAPI) VM(_ context.Context, u int64, id string) (controlclient.VM, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.getErr {
		return controlclient.VM{}, fmt.Errorf("transport")
	}
	if !a.vms[u][id] {
		return controlclient.VM{}, &controlclient.APIError{Code: "not_found", Status: 404}
	}
	return a.view(id), nil
}
func (a *fakeAPI) ListVMs(_ context.Context, u int64) ([]controlclient.VM, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr {
		return nil, fmt.Errorf("transport")
	}
	var l []controlclient.VM
	for id := range a.vms[u] {
		l = append(l, a.view(id))
	}
	return l, nil
}
func (a *fakeAPI) SSH(_ context.Context, u int64, id string) (controlclient.SSH, error) {
	if a.sshErr {
		return controlclient.SSH{}, fmt.Errorf("transport")
	}
	return controlclient.SSH{Host: "185.70.184.64", Port: 22001, User: "vm"}, nil
}
func (a *fakeAPI) SSHKey(_ context.Context, u int64, id string) (controlclient.GeneratedKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keyCalls++
	if a.noKey[id] {
		return controlclient.GeneratedKey{}, &controlclient.APIError{Code: "key_not_generated", Status: 409}
	}
	vm := id
	if a.keyVMID != "" {
		vm = a.keyVMID
	}
	return controlclient.GeneratedKey{VMID: vm, PublicKey: "ssh-ed25519 AAAA", PrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nSECRETKEYMATERIAL\n-----END OPENSSH PRIVATE KEY-----\n"}, nil
}

type env struct {
	t     *testing.T
	nx    *nxtest.Fake
	api   *fakeAPI
	m     *Manager
	store *Store
	dir   string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	creds := filepath.Join(dir, "admin.json")
	if e := os.WriteFile(creds, []byte(`{"username":"root","password":"adminpw"}`), 0600); e != nil {
		t.Fatal(e)
	}
	f := nxtest.New()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, e := OpenStore(filepath.Join(dir, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	c, e := NewClient(srv.URL, nil)
	if e != nil {
		t.Fatal(e)
	}
	api := &fakeAPI{vms: map[int64]map[string]bool{7: {"vm-a": true}, 8: {"vm-b": true}}, noKey: map[string]bool{}, expired: map[string]bool{}}
	cfg := Config{Enabled: true, URL: srv.URL, AdminCredentialsFile: creds, StateFile: filepath.Join(dir, "state.json")}
	m, e := NewManager(cfg, c, st, api)
	if e != nil {
		t.Fatal(e)
	}
	return &env{t, f, api, m, st, dir}
}

func TestRunRetriesAtStartupAndStopsBeforeStoreClose(t *testing.T) {
	v := setup(t)
	if err := v.m.Track(7, "vm-a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); v.m.Run(ctx) }()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		if rec, ok := v.store.Get("vm-a"); ok && rec.Stage == StageReady {
			break
		}
		select {
		case <-deadline.C:
			cancel()
			<-done
			t.Fatal("startup retry waited for ticker")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run ignored cancellation")
	}
	// Read external fake state only after the background worker has joined.
	if v.nx.NonAdmin() != 1 || v.nx.GlobalLogout {
		t.Fatal("startup retry violated account/session isolation")
	}
}

func TestEnsureCreatesOrdinaryAccountOnceAndIsIdempotent(t *testing.T) {
	v := setup(t)
	r, e := v.m.Ensure(context.Background(), 7, "vm-a")
	if e != nil || r.Status != Ready {
		t.Fatalf("%v %+v", e, r)
	}
	if len(r.Username) != 12 || !strings.HasPrefix(r.Username, "fx") || len(r.Password.Reveal()) < 24 {
		t.Fatalf("bad creds %q", r.Username)
	}
	r2, _ := v.m.Ensure(context.Background(), 7, "vm-a")
	if r2.Username != r.Username || r2.Password != r.Password || v.nx.NonAdmin() != 1 {
		t.Fatal("duplicate account or changed credentials")
	}
	for id, u := range v.nx.Users {
		if u.Name == r.Username {
			if u.Admin || len(v.nx.Identities[id]) != 1 || len(v.nx.Entries[id]) != 1 {
				t.Fatal("account not ordinary or objects missing")
			}
			for _, en := range v.nx.Entries[id] {
				cfg := en["config"].(map[string]any)
				if cfg["ip"] != "185.70.184.64" || cfg["port"].(float64) != 22001 {
					t.Fatal("entry endpoint")
				}
			}
		}
	}
	for tok, id := range v.nx.Sessions { // no admin or impersonated session survives
		t.Fatalf("leaked session %s for %d", tok, id)
	}
	st, _ := os.Stat(filepath.Join(v.dir, "state.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatalf("state mode %v", st.Mode())
	}
	raw, _ := os.ReadFile(filepath.Join(v.dir, "state.json"))
	if strings.Contains(string(raw), "SECRETKEYMATERIAL") || strings.Contains(string(raw), "BEGIN") {
		t.Fatal("private key persisted")
	}
}

func TestDomainErrorOn200ThenRetryUsesSameCredentials(t *testing.T) {
	v := setup(t)
	v.nx.FailCreate = true
	r, e := v.m.Ensure(context.Background(), 7, "vm-a")
	if e != nil || r.Status != Pending || r.Reason != "nexterm_rejected" {
		t.Fatalf("%v %+v", e, r)
	}
	rec, _ := v.store.Get("vm-a")
	user, pass := rec.Username, rec.Password
	v.nx.FailCreate = false
	v.m.next = map[string]time.Time{}
	v.m.Retry(context.Background())
	rec, _ = v.store.Get("vm-a")
	if rec.Stage != StageReady || rec.Username != user || rec.Password != pass || v.nx.NonAdmin() != 1 {
		t.Fatalf("retry changed credentials or duplicated: %+v", rec)
	}
}

func TestCrashAfterAccountCreateAdoptsOnlyWithMatchingPassword(t *testing.T) {
	v := setup(t)
	// Crash window: planned record + account exist, no IDs persisted.
	_ = v.store.Put(Record{VMID: "vm-a", Owner: 7, Username: "fxabcdefghij", Password: "pw-one-pw-one-pw-one-pw-one", Stage: StagePlanned})
	v.nx.Users[50] = &nxtest.User{Name: "fxabcdefghij", Pass: "pw-one-pw-one-pw-one-pw-one"}
	r, _ := v.m.Ensure(context.Background(), 7, "vm-a")
	if r.Status != Ready || v.nx.NonAdmin() != 1 {
		t.Fatalf("%+v", r)
	}
	// Foreign account with the same name but a different password is not adopted.
	_ = v.store.Put(Record{VMID: "vm-b", Owner: 8, Username: "fxzzzzzzzzzz", Password: "mine-mine-mine-mine-mine1", Stage: StagePlanned})
	v.nx.Users[51] = &nxtest.User{Name: "fxzzzzzzzzzz", Pass: "other"}
	r, _ = v.m.Ensure(context.Background(), 8, "vm-b")
	if r.Status != Pending || len(v.nx.Identities[51]) != 0 {
		t.Fatalf("adopted foreign account: %+v", r)
	}
}

func TestUserSuppliedKeyIsUnsupportedWithoutAccount(t *testing.T) {
	v := setup(t)
	v.api.noKey["vm-a"] = true
	r, e := v.m.Ensure(context.Background(), 7, "vm-a")
	if e != nil || r.Status != Unsupported || v.nx.NonAdmin() != 0 || r.Password != "" {
		t.Fatalf("%v %+v", e, r)
	}
	calls := v.api.keyCalls
	_, _ = v.m.Ensure(context.Background(), 7, "vm-a")
	if v.api.keyCalls != calls {
		t.Fatal("unsupported verdict not durable")
	}
}

func TestWrongKeyOrReadbackNeverReady(t *testing.T) {
	v := setup(t)
	v.api.keyVMID = "vm-other"
	if r, _ := v.m.Ensure(context.Background(), 7, "vm-a"); r.Status != Pending || v.nx.NonAdmin() != 0 {
		t.Fatalf("key for another VM accepted: %+v", r)
	}
	v.api.keyVMID = ""
	v.nx.BadEntryIP = true
	v.m.next = map[string]time.Time{}
	if r, _ := v.m.Ensure(context.Background(), 7, "vm-a"); r.Status != Pending {
		t.Fatalf("wrong readback accepted: %+v", r)
	}
	v.nx.AdminImpers = true
	if r, _ := v.m.Ensure(context.Background(), 7, "vm-a"); r.Status == Ready {
		t.Fatal("admin account accepted")
	}
}

func TestOwnerIsolation(t *testing.T) {
	v := setup(t)
	if _, e := v.m.Ensure(context.Background(), 7, "vm-a"); e != nil {
		t.Fatal(e)
	}
	if _, e := v.m.Ensure(context.Background(), 8, "vm-a"); e != ErrNotOwner {
		t.Fatalf("other owner got %v", e)
	}
	if _, e := v.m.Ensure(context.Background(), 8, "vm-zzz"); e != ErrNotOwner {
		t.Fatalf("unknown VM: %v", e)
	}
}

func TestReconcileDeletesOnlyProvablyGoneVMs(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	ra, _ := v.m.Ensure(ctx, 7, "vm-a")
	_, _ = v.m.Ensure(ctx, 8, "vm-b")
	v.nx.Users[99] = &nxtest.User{Name: "unrelated", Pass: "x"}
	delete(v.api.vms[7], "vm-a") // TTL/API delete outside the bot
	v.api.listErr = true
	if v.m.ReconcileNow(ctx) != 0 || v.nx.NonAdmin() != 3 {
		t.Fatal("deleted on transient list failure")
	}
	v.api.listErr = false
	v.api.getErr = true
	if v.m.ReconcileNow(ctx) != 0 || v.nx.NonAdmin() != 3 {
		t.Fatal("deleted on transient lookup failure")
	}
	v.api.getErr = false
	if n := v.m.ReconcileNow(ctx); n != 1 {
		t.Fatalf("deleted %d", n)
	}
	for _, u := range v.nx.Users {
		if u.Name == ra.Username {
			t.Fatal("account survived")
		}
	}
	if _, ok := v.store.Get("vm-a"); ok || v.nx.NonAdmin() != 2 || len(v.nx.Sessions) != 0 {
		t.Fatal("binding/other accounts/sessions wrong")
	}
	if _, ok := v.store.Get("vm-b"); !ok {
		t.Fatal("live VM binding removed")
	}
}

func TestCleanupKeepsBindingWhenNextermDown(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	_, _ = v.m.Ensure(ctx, 7, "vm-a")
	delete(v.api.vms[7], "vm-a")
	v.nx.Users[1].Pass = "rotated" // admin login now fails
	if v.m.ReconcileNow(ctx) != 0 {
		t.Fatal("deleted without admin")
	}
	if _, ok := v.store.Get("vm-a"); !ok {
		t.Fatal("binding dropped, orphan account unrecoverable")
	}
}

func TestPasswordRedaction(t *testing.T) {
	v := setup(t)
	r, _ := v.m.Ensure(context.Background(), 7, "vm-a")
	pw := r.Password.Reveal()
	rec, _ := v.store.Get("vm-a")
	for _, s := range []string{fmt.Sprintf("%v", r), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", r), fmt.Sprint(r.Password), fmt.Sprintf("%v", rec)} {
		if strings.Contains(s, pw) {
			t.Fatalf("password leaked: %s", s)
		}
	}
	if b, _ := json.Marshal(r); strings.Contains(string(b), pw) {
		t.Fatal("json leak")
	}
}

func TestStoreRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	p := filepath.Join(dir, "state.json")
	_ = os.WriteFile(p, []byte(`{"version":1,"records":[]}`), 0644)
	if _, e := OpenStore(p); e == nil {
		t.Fatal("group/world readable state accepted")
	}
	_ = os.Remove(p)
	_ = os.Remove(p + ".lock")
	target := filepath.Join(dir, "elsewhere")
	_ = os.WriteFile(target, []byte(`{"version":1,"records":[]}`), 0600)
	_ = os.Symlink(target, p)
	if _, e := OpenStore(p); e == nil {
		t.Fatal("symlink state accepted")
	}
	loose := t.TempDir()
	_ = os.Chmod(loose, 0755)
	if _, e := OpenStore(filepath.Join(loose, "s.json")); e == nil {
		t.Fatal("loose directory accepted")
	}
}

func TestConfigValidation(t *testing.T) {
	var nilCfg *Config
	if nilCfg.Validate() != nil || (&Config{}).Validate() != nil {
		t.Fatal("disabled must validate")
	}
	bad := []Config{
		{Enabled: true, URL: "http://example.com", AdminCredentialsFile: "/a/b", StateFile: "/a/c"},
		{Enabled: true, URL: "https://u:p@example.com", AdminCredentialsFile: "/a/b", StateFile: "/a/c"},
		{Enabled: true, URL: "https://example.com/x", AdminCredentialsFile: "/a/b", StateFile: "/a/c"},
		{Enabled: true, URL: "https://example.com", AdminCredentialsFile: "rel", StateFile: "/a/c"},
		{Enabled: true, URL: "https://example.com", AdminCredentialsFile: "/a/b", StateFile: "/a/b"},
		{Enabled: false, URL: "https://example.com"},
	}
	for i, c := range bad {
		if c.Validate() == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	if (&Config{Enabled: true, URL: "https://example.com:19454", AdminCredentialsFile: "/a/b", StateFile: "/a/c"}).Validate() != nil {
		t.Fatal("good config rejected")
	}
}

func TestCloseSessionIsExactAndNeverGlobalLogout(t *testing.T) {
	v := setup(t)
	v.nx.Users[60] = &nxtest.User{Name: "someone", Pass: "p"}
	real := v.nx.Login(60) // a live terminal/browser session of the same account
	mine := v.nx.Login(60)
	if e := v.m.nx.CloseSession(context.Background(), mine); e != nil {
		t.Fatal(e)
	}
	if _, ok := v.nx.Sessions[mine]; ok {
		t.Fatal("session not closed")
	}
	if _, ok := v.nx.Sessions[real]; !ok || v.nx.GlobalLogout {
		t.Fatal("other live session destroyed / global logout used")
	}
}

func TestAdoptionLoginPreservesRealSession(t *testing.T) {
	v := setup(t)
	_ = v.store.Put(Record{VMID: "vm-a", Owner: 7, Username: "fxabcdefghij", Password: "pw-one-pw-one-pw-one-pw-one", Stage: StagePlanned})
	v.nx.Users[50] = &nxtest.User{Name: "fxabcdefghij", Pass: "pw-one-pw-one-pw-one-pw-one"}
	real := v.nx.Login(50)
	if r, _ := v.m.Ensure(context.Background(), 7, "vm-a"); r.Status != Ready {
		t.Fatalf("%+v", r)
	}
	if _, ok := v.nx.Sessions[real]; !ok || v.nx.GlobalLogout || len(v.nx.Sessions) != 1 {
		t.Fatalf("sessions %v global=%v", v.nx.Sessions, v.nx.GlobalLogout)
	}
}

func TestCleanupFailsClosed(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(v *env){
		"impersonation fails": func(v *env) { v.nx.FailImpers = true },
		"isAdmin missing":     func(v *env) { v.nx.OmitIsAdmin = true },
		"admin claim":         func(v *env) { v.nx.AdminImpers = true },
		"id changed": func(v *env) {
			rec, _ := v.store.Get("vm-a")
			rec.UserID += 1000
			_ = v.store.Put(rec)
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			v := setup(t)
			if r, _ := v.m.Ensure(ctx, 7, "vm-a"); r.Status != Ready {
				t.Fatal("setup")
			}
			delete(v.api.vms[7], "vm-a")
			mut(v)
			if v.m.ReconcileNow(ctx) != 0 || v.nx.NonAdmin() != 1 {
				t.Fatal("account deleted without proof")
			}
			if _, ok := v.store.Get("vm-a"); !ok {
				t.Fatal("binding forgotten")
			}
		})
	}
	t.Run("crash before id with conflicting foreign account", func(t *testing.T) {
		v := setup(t)
		_ = v.store.Put(Record{VMID: "vm-a", Owner: 7, Username: "fxforeignxxx", Password: "mine-mine-mine-mine-mine1", Stage: StagePlanned})
		v.nx.Users[70] = &nxtest.User{Name: "fxforeignxxx", Pass: "theirs"}
		delete(v.api.vms[7], "vm-a")
		if v.m.ReconcileNow(ctx) != 0 || v.nx.Users[70] == nil {
			t.Fatal("foreign account deleted")
		}
	})
	t.Run("crash before id with stored password proof", func(t *testing.T) {
		v := setup(t)
		_ = v.store.Put(Record{VMID: "vm-a", Owner: 7, Username: "fxmineminemi", Password: "mine-mine-mine-mine-mine1", Stage: StagePlanned})
		v.nx.Users[71] = &nxtest.User{Name: "fxmineminemi", Pass: "mine-mine-mine-mine-mine1"}
		real := v.nx.Login(71)
		delete(v.api.vms[7], "vm-a")
		if v.m.ReconcileNow(ctx) != 1 || v.nx.Users[71] != nil {
			t.Fatal("proven account not removed")
		}
		_ = real
		if _, ok := v.store.Get("vm-a"); ok {
			t.Fatal("binding kept after verified delete")
		}
	})
}

func TestExpiredReadyBindingIsNeitherReusedNorDelivered(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	r, _ := v.m.Ensure(ctx, 7, "vm-a")
	if r.Status != Ready || len(v.m.Undelivered(ctx)) != 1 {
		t.Fatal("setup")
	}
	v.api.expired["vm-a"] = true // TTL passed, VM still listed
	if _, e := v.m.Ensure(ctx, 7, "vm-a"); e != ErrVMInactive {
		t.Fatalf("expired VM reissued credentials: %v", e)
	}
	if len(v.m.Undelivered(ctx)) != 0 {
		t.Fatal("stale credentials deliverable")
	}
	if v.m.ReconcileNow(ctx) != 1 || v.nx.NonAdmin() != 0 {
		t.Fatal("expired VM account not cleaned")
	}
	// A transient API error must not downgrade or delete a ready binding.
	v2 := setup(t)
	_, _ = v2.m.Ensure(ctx, 7, "vm-a")
	v2.api.getErr = true
	if r, _ := v2.m.Ensure(ctx, 7, "vm-a"); r.Status != Pending {
		t.Fatal("transient error served credentials")
	}
	if rec, _ := v2.store.Get("vm-a"); rec.Stage != StageReady {
		t.Fatal("ready binding downgraded")
	}
}

func TestTrackedCreateIsRecoveredByRetry(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	if e := v.m.Track(7, "vm-a"); e != nil || v.nx.NonAdmin() != 0 {
		t.Fatal(e)
	}
	if v.m.Track(8, "vm-a") != ErrNotOwner {
		t.Fatal("tracked for another owner")
	}
	v.api.sshErr = true // create operation still running
	v.m.Retry(ctx)
	if rec, _ := v.store.Get("vm-a"); rec.Stage != StagePlanned {
		t.Fatal("not pending")
	}
	v.api.sshErr = false
	v.m.next = map[string]time.Time{}
	v.m.Retry(ctx)
	if rec, _ := v.store.Get("vm-a"); rec.Stage != StageReady || v.nx.NonAdmin() != 1 {
		t.Fatalf("not recovered: %+v", rec)
	}
	// A tracked create that never produced a VM is dropped without any account.
	_ = v.m.Track(7, "vm-ghost")
	if v.m.ReconcileNow(ctx) != 1 {
		t.Fatal("ghost binding kept")
	}
}
