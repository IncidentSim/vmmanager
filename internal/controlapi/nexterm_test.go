package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/nextermaccess"
	"github.com/petruekhin/vibe-firecracker/internal/nextermaccess/nxtest"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type pemKeys struct{ generated bool }

func (k pemKeys) GeneratedKey(_ context.Context, _ int64, _ string) (GeneratedKey, error) {
	if !k.generated {
		return GeneratedKey{}, ErrKeyNotGenerated
	}
	return GeneratedKey{PrivateKeyPEM: []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"), PublicKey: "ssh-ed25519 AAAA"}, nil
}

type nxEnv struct {
	srv *httptest.Server
	nx  *nxtest.Fake
	st  *vmbot.Store
	b   *NextermBrowser
}

func nextermAPI(t *testing.T, generated bool) *nxEnv {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	creds := filepath.Join(dir, "admin.json")
	if e := os.WriteFile(creds, []byte(`{"username":"root","password":"adminpw"}`), 0600); e != nil {
		t.Fatal(e)
	}
	fake := nxtest.New()
	nsrv := httptest.NewServer(fake)
	t.Cleanup(nsrv.Close)
	nc, e := nextermaccess.NewClient(nsrv.URL, nil)
	if e != nil {
		t.Fatal(e)
	}
	nstore, e := nextermaccess.OpenStore(filepath.Join(dir, "nx-state.json"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { nstore.Close() })
	st, e := vmbot.OpenControlStore(filepath.Join(dir, "state"), 0)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	if e = st.Update(func(s *vmbot.State) {
		s.Generation = 1
		s.VMs = []vmbot.VMRecord{{ID: "vm-a", Token: strings.Repeat("a", 43), Generation: 1, Phase: vmbot.PhaseActive, OwnerUserID: 11}}
		s.Operations = []vmbot.ControlOperation{{ID: "op-t", OwnerUserID: 11, Key: "k", Digest: digest("terminal"), Kind: "terminal", VMID: "vm-a", Status: "succeeded"}}
	}); e != nil {
		t.Fatal(e)
	}
	lc := &sshLifecycle{info: vmlib.SSHInfo{Host: "10.0.0.5", Port: 22, User: "root"}, state: vmlib.StateRunning}
	ctl := vmbot.NewController(lc, st, &terms{}, vmbot.ControllerConfig{QueryTimeout: 200 * time.Millisecond, PublicOrigin: "https://ttyd.example"})
	ctl.ConfigureControl(vmbot.ControlPolicy{AllowedUsers: []int64{11, 22}, MaxVMs: 2, MaxVMsPerUser: 1})
	cfg := nextermaccess.Config{Enabled: true, URL: nsrv.URL, AdminCredentialsFile: creds, StateFile: filepath.Join(dir, "nx-state.json")}
	b, e := NewNextermBrowser(cfg, nc, nstore, ctl, pemKeys{generated})
	if e != nil {
		t.Fatal(e)
	}
	h, e := New(ctl, []Credential{{SHA256: digest("one"), UserID: 11}, {SHA256: digest("two"), UserID: 22}})
	if e != nil {
		t.Fatal(e)
	}
	if e = h.Configure(Options{Browser: b}); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &nxEnv{srv, fake, st, b}
}

func (v *nxEnv) get(t *testing.T, path, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", v.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func checkLink(t *testing.T, v *nxEnv, raw string) {
	t.Helper()
	u, e := url.Parse(raw)
	if e != nil || u.Path != "/servers" {
		t.Fatalf("bad link %v", e)
	}
	q := u.Query()
	if q.Get("connectId") == "" || q.Get("connectFtpId") != q.Get("connectId") {
		t.Fatalf("both connect IDs required and equal: %q", raw)
	}
	if id, ok := v.nx.Sessions[q.Get("token")]; !ok || v.nx.Users[id].Admin {
		t.Fatal("link token must be an ordinary per-VM account session, never the admin's")
	}
	if strings.Contains(raw, "adminpw") || strings.Contains(raw, "PRIVATE KEY") {
		t.Fatal("secret leaked into link")
	}
}

func TestSSHAddsBrowserURLKeepsRawContractAndIsOwnerOnly(t *testing.T) {
	v := nextermAPI(t, true)
	code, out := v.get(t, "/v1/vms/vm-a/ssh", "one")
	if code != 200 || out["host"] != "10.0.0.5" || out["user"] != "root" || out["port"] != float64(22) || out["browser_status"] != "ready" {
		t.Fatalf("%d %v", code, out)
	}
	if _, ok := out["host_key_fingerprint"]; !ok {
		t.Fatal("raw contract field missing")
	}
	checkLink(t, v, out["browser_url"].(string))
	if strings.Contains(out["browser_url"].(string), "ttyd.example") || strings.Contains(out["browser_url"].(string), "/t/") {
		t.Fatal("ttyd URL")
	}
	// Reuses the one account and ordinary session.
	_, again := v.get(t, "/v1/vms/vm-a/ssh", "one")
	if again["browser_url"] != out["browser_url"] || v.nx.NonAdmin() != 1 || len(v.nx.Sessions) != 1 {
		t.Fatal("per-VM account/session not reused")
	}
	// Another admitted user gets nothing, and no second account appears.
	if code, other := v.get(t, "/v1/vms/vm-a/ssh", "two"); code != 404 || other["browser_url"] != nil {
		t.Fatalf("foreign owner: %d %v", code, other)
	}
	if v.nx.NonAdmin() != 1 {
		t.Fatal("foreign request provisioned an account")
	}
}

func TestTerminalOperationResultIsNextermLinkNotTTYD(t *testing.T) {
	v := nextermAPI(t, true)
	code, out := v.get(t, "/v1/operations/op-t", "one")
	res, _ := out["result"].(map[string]any)
	if code != 200 || res["browser_status"] != "ready" {
		t.Fatalf("%d %v", code, out)
	}
	checkLink(t, v, res["url"].(string))
	if code, _ := v.get(t, "/v1/operations/op-t", "two"); code != 404 {
		t.Fatal("operation visible to another owner")
	}
}

func TestUserSuppliedKeyBrowserUnsupportedRawSSHStillWorks(t *testing.T) {
	v := nextermAPI(t, false)
	code, out := v.get(t, "/v1/vms/vm-a/ssh", "one")
	if code != 200 || out["host"] != "10.0.0.5" || out["browser_status"] != "unsupported" || out["browser_url"] != nil {
		t.Fatalf("%d %v", code, out)
	}
	_, op := v.get(t, "/v1/operations/op-t", "one")
	res, _ := op["result"].(map[string]any)
	if res["browser_status"] != "unsupported" || res["url"] != nil || v.nx.NonAdmin() != 0 {
		t.Fatalf("%v", op)
	}
}

// Deleting the VM durably revokes the account and its link session.
func TestCleanupRevokesAccountAndSession(t *testing.T) {
	v := nextermAPI(t, true)
	_, out := v.get(t, "/v1/vms/vm-a/ssh", "one")
	tok := mustToken(t, out["browser_url"].(string))
	if e := v.st.Update(func(s *vmbot.State) { s.VMs = nil }); e != nil {
		t.Fatal(e)
	}
	if n := v.b.Manager().ReconcileNow(context.Background()); n != 1 {
		t.Fatalf("reconciled %d", n)
	}
	if _, ok := v.nx.Sessions[tok]; ok || v.nx.NonAdmin() != 0 {
		t.Fatal("account/session survived VM deletion")
	}
}

func TestExpiredSSHDoesNotReturnBrowserLinkAndReconciles(t *testing.T) {
	v := nextermAPI(t, true)
	_, out := v.get(t, "/v1/vms/vm-a/ssh", "one")
	tok := mustToken(t, out["browser_url"].(string))
	past := time.Now().Add(-time.Minute)
	if err := v.st.Update(func(s *vmbot.State) { s.VMs[0].ExpiresAt = &past }); err != nil {
		t.Fatal(err)
	}
	code, expired := v.get(t, "/v1/vms/vm-a/ssh", "one")
	if code != 200 || expired["host"] != "10.0.0.5" || expired["browser_url"] != nil {
		t.Fatal("expired VM returned browser access or lost raw SSH")
	}
	if v.b.Manager().ReconcileNow(context.Background()) != 1 {
		t.Fatal("TTL account not reconciled")
	}
	if _, ok := v.nx.Sessions[tok]; ok || v.nx.NonAdmin() != 0 || v.nx.GlobalLogout {
		t.Fatal("TTL revocation failed or global logout used")
	}
}

func TestNextermOutagePendingPreservesRawSSHAndResumes(t *testing.T) {
	v := nextermAPI(t, true)
	v.nx.Mu.Lock()
	v.nx.FailCreate = true
	v.nx.Mu.Unlock()
	code, out := v.get(t, "/v1/vms/vm-a/ssh", "one")
	if code != 200 || out["host"] != "10.0.0.5" || out["browser_status"] != BrowserPending || out["browser_url"] != nil {
		t.Fatal("outage broke raw SSH or exposed fallback link")
	}
	v.nx.Mu.Lock()
	v.nx.FailCreate = false
	v.nx.Mu.Unlock()
	code, out = v.get(t, "/v1/vms/vm-a/ssh", "one")
	if code != 200 || out["browser_status"] != BrowserReady {
		t.Fatal("provisioning did not resume")
	}
	checkLink(t, v, out["browser_url"].(string))
	if v.nx.NonAdmin() != 1 || v.nx.GlobalLogout {
		t.Fatal("resumption duplicated account or globally logged out")
	}
}

func mustToken(t *testing.T, raw string) string {
	t.Helper()
	u, e := url.Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	return u.Query().Get("token")
}

func TestNextermConfigValidationAndStrictDisabled(t *testing.T) {
	c := &nextermaccess.Config{Enabled: false, URL: "https://x.example"}
	if c.Validate() == nil {
		t.Fatal("disabled config must carry no settings")
	}
	c = &nextermaccess.Config{Enabled: true, URL: "https://nx.example", AdminCredentialsFile: "relative.json", StateFile: "/var/lib/x/s.json"}
	if c.Validate() == nil {
		t.Fatal("relative private path accepted")
	}
}
