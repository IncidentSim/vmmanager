package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/nextermaccess"
)

// Opt-in live Nexterm acceptance (docs/nexterm-access.md, "Live acceptance
// test"). It never reads the Telegram API; it drives the same Manager the bot
// uses. Nothing secret (login, password, private key, token, HTTP bodies) is
// ever logged: failures carry only fixed text, status codes and resource IDs.
const (
	envNextermGate  = "VMAPI_E2E_NEXTERM"
	envNextermURL   = "NEXTERM_E2E_URL"
	envNextermAdmin = "NEXTERM_E2E_ADMIN_FILE"

	nextermReadyWait = 2 * time.Minute // bounded wait for Ensure + real SSH exec
	nextermAccountOp = 30 * time.Second
)

type nextermConfig struct {
	URL       string
	AdminFile string
}

// loadNextermConfig validates settings (names only in errors, never values).
// It checks the admin file is an owned-by-us 0600 regular file but never reads it.
func loadNextermConfig(getenv func(string) string) (nextermConfig, error) {
	c := nextermConfig{URL: strings.TrimRight(strings.TrimSpace(getenv(envNextermURL)), "/"), AdminFile: strings.TrimSpace(getenv(envNextermAdmin))}
	var missing []string
	if c.URL == "" {
		missing = append(missing, envNextermURL)
	}
	if c.AdminFile == "" {
		missing = append(missing, envNextermAdmin)
	}
	if len(missing) > 0 {
		return nextermConfig{}, fmt.Errorf("missing required environment: %s", strings.Join(missing, ", "))
	}
	if nextermaccess.ValidateURL(c.URL) != nil {
		return nextermConfig{}, fmt.Errorf("%s must be a bare https origin (http only on loopback)", envNextermURL)
	}
	if !filepath.IsAbs(c.AdminFile) || filepath.Clean(c.AdminFile) != c.AdminFile {
		return nextermConfig{}, fmt.Errorf("%s must be an absolute clean path", envNextermAdmin)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(c.AdminFile, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG ||
		st.Mode&0o077 != 0 || st.Uid != uint32(os.Geteuid()) {
		return nextermConfig{}, fmt.Errorf("%s must be an owned regular file with mode 0600 (no symlink)", envNextermAdmin)
	}
	return c, nil
}

// nextermExec performs the real Nexterm engine call
// POST /api/connections/{entryID}/exec with the entry's identity. It returns
// stdout only on {"success":true}; error text never includes response bodies.
func nextermExec(ctx context.Context, hc *http.Client, base, token string, entryID, identityID int64, command string) (string, error) {
	body, _ := json.Marshal(map[string]any{"identityId": identityID, "command": command})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/connections/%d/exec", base, entryID), bytes.NewReader(body))
	if err != nil {
		return "", errors.New("invalid exec request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("exec transport failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil || len(b) > 1<<20 {
		return "", errors.New("exec response unreadable")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exec HTTP %d", resp.StatusCode)
	}
	var out struct {
		Success bool   `json:"success"`
		Stdout  string `json:"stdout"`
	}
	if json.Unmarshal(b, &out) != nil {
		return "", errors.New("exec response not JSON")
	}
	if !out.Success {
		return "", errors.New("exec reported failure")
	}
	return out.Stdout, nil
}

func TestLiveNexterm(t *testing.T) {
	if os.Getenv(envNextermGate) != "1" {
		t.Skip("live Nexterm acceptance disabled; set VMAPI_E2E=1 VMAPI_E2E_NEXTERM=1 (see docs/nexterm-access.md)")
	}
	if os.Getenv(envGate) != "1" {
		t.Skip("live Nexterm acceptance needs VMAPI_E2E=1 as well (see docs/e2e-live.md)")
	}
	nc, err := loadNextermConfig(os.Getenv)
	if err != nil {
		t.Fatalf("live Nexterm configuration: %v", err)
	}

	// Cleanups are LIFO. Registered first => run last: temp dir removal, then
	// store close, then account cleanup, and newSuite's VM cleanup (registered
	// after these) runs before all of them. So VMs are deleted first, then the
	// Nexterm account is reconciled away, then the store is closed.
	state := t.TempDir() // MkdirTemp: mode 0700, removed last
	stateFile := filepath.Join(state, "state.json")
	var store *nextermaccess.Store
	t.Cleanup(func() {
		if store != nil {
			_ = store.Close()
		}
	})
	var mgr *nextermaccess.Manager
	t.Cleanup(func() {
		if mgr == nil || store == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), nextermAccountOp)
		defer cancel()
		mgr.ReconcileNow(ctx)
		if left := store.All(); len(left) > 0 {
			for _, r := range left {
				t.Errorf("CLEANUP PROBLEM: Nexterm account binding for VM %s may remain (stage %s)", r.VMID, r.Stage)
			}
		}
	})

	s := newSuite(t)
	store, err = nextermaccess.OpenStore(stateFile)
	if err != nil {
		t.Fatalf("open test state: %v", err)
	}
	nx, err := nextermaccess.NewClient(nc.URL, &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	if err != nil {
		t.Fatalf("nexterm client: %v", err)
	}
	mgr, err = nextermaccess.NewManager(nextermaccess.Config{Enabled: true, URL: nc.URL, AdminCredentialsFile: nc.AdminFile, StateFile: stateFile}, nx, store, s.c)
	if err != nil {
		t.Fatalf("nexterm manager: %v", err)
	}

	s.preflight()
	// No SSHPublicKey in the request: the API generates the key (owner-authorized
	// /ssh-key), which is the only mode that gets a Nexterm account.
	a := s.createActive("nexterm", s.cfg.BaseImageID)
	vmID := a.VMID
	if err := mgr.Track(s.owner, vmID); err != nil {
		t.Fatalf("track: %v", err)
	}

	var res nextermaccess.Result
	ctx, cancel := context.WithTimeout(s.ctx, nextermReadyWait)
	defer cancel()
	err = pollUntil(ctx, pollEvery, func(ctx context.Context) (bool, error) {
		r, e := mgr.Ensure(ctx, s.owner, vmID)
		if e != nil {
			return false, permanent(fmt.Errorf("ensure: %w", e))
		}
		switch r.Status {
		case nextermaccess.Ready:
			res = r
			return true, nil
		case nextermaccess.Unsupported:
			return false, permanent(errors.New("VM not eligible for Nexterm (reason: " + r.Reason + ")"))
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Nexterm account not ready: %v", err)
	}
	rec, ok := store.Get(vmID)
	if !ok || rec.EntryID == 0 || rec.IdentityID == 0 || rec.Username == "" || res.Username != rec.Username {
		t.Fatal("binding incomplete after ready")
	}
	user, pass := rec.Username, nextermaccess.Secret(rec.Password)

	// Ordinary-account login (random account, not the admin).
	tok, err := nx.Login(s.ctx, user, pass)
	if err != nil {
		t.Fatalf("ordinary account login: %v", err)
	}
	defer func() {
		cctx, cc := context.WithTimeout(context.Background(), nextermAccountOp)
		defer cc()
		_ = nx.CloseSession(cctx, tok)
	}()
	if me, err := nx.Me(s.ctx, tok); err != nil || me.IsAdmin || me.Username != user {
		t.Fatalf("login is not the expected ordinary account: %v", err)
	}
	if id, err := nx.FindEntry(s.ctx, tok, "vm-"+s.n.name("nexterm")); err != nil || id != rec.EntryID {
		t.Fatalf("entry not visible to the ordinary account: %v", err)
	}

	// Real SSH through the Nexterm engine, polled while the guest sshd settles.
	marker := "NEXTERM_E2E_" + randHex(8)
	hc := &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ectx, ecancel := context.WithTimeout(s.ctx, nextermReadyWait)
	defer ecancel()
	var last error
	err = pollUntil(ectx, pollEvery, func(ctx context.Context) (bool, error) {
		out, e := nextermExec(ctx, hc, nc.URL, tok, rec.EntryID, rec.IdentityID, "printf "+marker)
		if e != nil {
			last = e
			return false, nil
		}
		if out != marker {
			last = errors.New("exec stdout did not match marker")
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Nexterm engine SSH exec did not succeed in time: %v (last: %v)", err, last)
	}
	t.Logf("Nexterm engine executed marker over SSH for VM %s", vmID)

	// Delete the VM, then reconcile: the account must be gone and login rejected.
	s.deleteVM("nexterm", vmID)
	dctx, dcancel := context.WithTimeout(context.Background(), nextermAccountOp)
	defer dcancel()
	if n := mgr.ReconcileNow(dctx); n != 1 {
		t.Fatalf("reconcile removed %d accounts, want 1", n)
	}
	if left := store.All(); len(left) != 0 {
		t.Fatalf("%d binding(s) remain after reconcile", len(left))
	}
	if tok2, err := nx.Login(dctx, user, pass); err == nil {
		_ = nx.CloseSession(dctx, tok2)
		t.Fatal("deleted account can still log in")
	}
}

func TestNextermConfigValidation(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "admin.json")
	if err := os.WriteFile(good, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	env := func(u, f string) func(string) string {
		return func(k string) string {
			switch k {
			case envNextermURL:
				return u
			case envNextermAdmin:
				return f
			}
			return ""
		}
	}
	if c, err := loadNextermConfig(env("https://nx.example.org:19454/", good)); err != nil || c.URL != "https://nx.example.org:19454" {
		t.Fatalf("valid config rejected: %+v %v", c, err)
	}
	for name, e := range map[string]func(string) string{
		"missing":  env("", ""),
		"http":     env("http://nx.example.org", good),
		"relative": env("https://nx.example.org", "admin.json"),
		"unclean":  env("https://nx.example.org", dir+"/./admin.json"),
		"loose":    env("https://nx.example.org", loose),
		"symlink":  env("https://nx.example.org", link),
		"absent":   env("https://nx.example.org", filepath.Join(dir, "none.json")),
	} {
		if _, err := loadNextermConfig(e); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), dir) {
			t.Errorf("%s: error leaks path value", name)
		}
	}
}

func TestNextermExec(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	reply := `{"success":true,"stdout":"M"}`
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.Method+" "+r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()
	out, err := nextermExec(context.Background(), srv.Client(), srv.URL, "tok", 7, 9, "printf M")
	if err != nil || out != "M" {
		t.Fatalf("exec: %q %v", out, err)
	}
	if gotAuth != "Bearer tok" || gotPath != "POST /api/connections/7/exec" || gotBody["identityId"] != float64(9) || gotBody["command"] != "printf M" {
		t.Fatalf("bad request: %q %q %v", gotAuth, gotPath, gotBody)
	}
	reply = `{"success":false,"stdout":"SECRET-BODY"}`
	if _, err = nextermExec(context.Background(), srv.Client(), srv.URL, "tok", 7, 9, "x"); err == nil || strings.Contains(err.Error(), "SECRET-BODY") {
		t.Fatalf("failure not reported safely: %v", err)
	}
	status, reply = http.StatusUnauthorized, "SECRET-BODY"
	if _, err = nextermExec(context.Background(), srv.Client(), srv.URL, "tok", 7, 9, "x"); err == nil || strings.Contains(err.Error(), "SECRET-BODY") {
		t.Fatalf("HTTP failure not reported safely: %v", err)
	}
}
