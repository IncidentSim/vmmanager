package nextermaccess

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func linkSetup(t *testing.T) (*env, Record) {
	t.Helper()
	v := setup(t)
	if r, e := v.m.Ensure(context.Background(), 7, "vm-a"); e != nil || r.Status != Ready {
		t.Fatalf("setup %v %+v", e, r)
	}
	rec, _ := v.store.Get("vm-a")
	return v, rec
}

func linkToken(t *testing.T, l Secret) string {
	t.Helper()
	u, e := url.Parse(l.Reveal())
	if e != nil {
		t.Fatal(e)
	}
	return u.Query().Get("token")
}

func TestPrepareLinkURLUserSessionNotAdminAndReused(t *testing.T) {
	v, rec := linkSetup(t)
	ctx := context.Background()
	l, e := v.m.PrepareLink(ctx, 7, "vm-a")
	if e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(l.Reveal())
	if e != nil || !strings.HasPrefix(l.Reveal(), v.m.cfg.URL+"/servers?token=") || u.Path != "/servers" {
		t.Fatalf("bad link shape %v", e)
	}
	tok, conn := u.Query().Get("token"), u.Query().Get("connectId")
	if u.Query().Get("sidebarCollapsed") != "true" || u.Query().Has("fullscreen") {
		t.Fatal("expected collapsed sidebar without forced fullscreen")
	}
	if u.Query().Get("splitLeft") != "sftp" || u.Query().Get("splitRatio") != "40" {
		t.Fatal("expected SFTP left 40 percent")
	}
	if u.Query().Get("splitMode") != "true" {
		t.Fatal("expected splitMode=true")
	}
	if ftp := u.Query().Get("connectFtpId"); ftp != conn || ftp == "" {
		t.Fatalf("connectFtpId %q must equal connectId %q", ftp, conn)
	}
	if conn != strconv.FormatInt(rec.EntryID, 10) || v.nx.Sessions[tok] != rec.UserID || v.nx.Users[rec.UserID].Admin {
		t.Fatal("token is not the ordinary account's session or wrong entry")
	}
	// Retries/polls reuse the same session: exactly one session exists, no admin one.
	for i := 0; i < 4; i++ {
		l2, e := v.m.PrepareLink(ctx, 7, "vm-a")
		if e != nil || l2.Reveal() != l.Reveal() {
			t.Fatalf("link not reused: %v", e)
		}
	}
	if len(v.nx.Sessions) != 1 || v.nx.GlobalLogout {
		t.Fatalf("session leak: %d", len(v.nx.Sessions))
	}
	if got, _ := v.store.Get("vm-a"); got.LinkToken != tok {
		t.Fatal("link session not durable")
	}
}

func TestPrepareLinkOwnerOnlyAndLiveness(t *testing.T) {
	v, _ := linkSetup(t)
	ctx := context.Background()
	if _, e := v.m.PrepareLink(ctx, 8, "vm-a"); e != ErrNotOwner {
		t.Fatalf("foreign owner got link: %v", e)
	}
	v.api.expired["vm-a"] = true
	if _, e := v.m.PrepareLink(ctx, 7, "vm-a"); e != ErrVMInactive {
		t.Fatalf("expired VM got link: %v", e)
	}
	delete(v.api.vms[7], "vm-a")
	if _, e := v.m.PrepareLink(ctx, 7, "vm-a"); e != ErrNotOwner {
		t.Fatalf("deleted VM got link: %v", e)
	}
	if len(v.nx.Sessions) != 0 {
		t.Fatal("session minted for refused request")
	}
	v2, _ := linkSetup(t)
	v2.api.getErr = true
	if _, e := v2.m.PrepareLink(ctx, 7, "vm-a"); e == nil || len(v2.nx.Sessions) != 0 {
		t.Fatal("link on API outage")
	}
}

func TestPrepareLinkRefusesAdminSessionWithoutLeakOrTokenInError(t *testing.T) {
	v, _ := linkSetup(t)
	v.nx.AdminImpers = true
	_, e := v.m.PrepareLink(context.Background(), 7, "vm-a")
	if e == nil || len(v.nx.Sessions) != 0 {
		t.Fatalf("admin-claiming session accepted or leaked: %v", e)
	}
	if strings.Contains(e.Error(), "tok") {
		t.Fatal("token in error")
	}
	if got, _ := v.store.Get("vm-a"); got.LinkToken != "" {
		t.Fatal("refused token persisted")
	}
}

func TestPrepareLinkReplacesDeadOrWrongStoredToken(t *testing.T) {
	v, _ := linkSetup(t)
	ctx := context.Background()
	l, _ := v.m.PrepareLink(ctx, 7, "vm-a")
	got, _ := v.store.Get("vm-a")
	delete(v.nx.Sessions, got.LinkToken) // expired server-side
	l2, e := v.m.PrepareLink(ctx, 7, "vm-a")
	if e != nil || l2.Reveal() == l.Reveal() || len(v.nx.Sessions) != 1 {
		t.Fatalf("dead token not replaced: %v %d", e, len(v.nx.Sessions))
	}
	// A stored token that belongs to the admin is closed individually, not reused.
	wrong := v.nx.Login(1)
	other := v.nx.Login(1)
	got, _ = v.store.Get("vm-a")
	got.LinkToken = wrong
	_ = v.store.Put(got)
	if _, e := v.m.PrepareLink(ctx, 7, "vm-a"); e != nil {
		t.Fatal(e)
	}
	if _, still := v.nx.Sessions[wrong]; still {
		t.Fatal("wrong-account link session left open")
	}
	if _, ok := v.nx.Sessions[other]; !ok || v.nx.GlobalLogout {
		t.Fatal("unrelated session killed / global logout used")
	}
}

func TestCleanupRevokesLinkSession(t *testing.T) {
	v, _ := linkSetup(t)
	ctx := context.Background()
	l, _ := v.m.PrepareLink(ctx, 7, "vm-a")
	tok := linkToken(t, l)
	delete(v.api.vms[7], "vm-a")
	if v.m.ReconcileNow(ctx) != 1 || v.nx.NonAdmin() != 0 {
		t.Fatal("not cleaned")
	}
	if _, ok := v.nx.Sessions[tok]; ok || len(v.nx.Sessions) != 0 || v.nx.GlobalLogout {
		t.Fatal("link session survived deletion")
	}
	if _, e := v.m.PrepareLink(ctx, 7, "vm-a"); e == nil {
		t.Fatal("link after deletion")
	}
}

func TestBrowserLinkUnifiedAndUnsupported(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	l, e := v.m.BrowserLink(ctx, 7, "vm-a")
	if e != nil || !strings.Contains(l.Reveal(), "&connectFtpId=") {
		t.Fatalf("%v", e)
	}
	l2, e := v.m.BrowserLink(ctx, 7, "vm-a")
	if e != nil || l2.Reveal() != l.Reveal() || len(v.nx.Sessions) != 1 {
		t.Fatal("ordinary session must be reused")
	}
	if _, e := v.m.BrowserLink(ctx, 8, "vm-a"); e == nil {
		t.Fatal("other owner must be refused")
	}
	v.api.noKey["vm-b"] = true
	if _, e := v.m.BrowserLink(ctx, 8, "vm-b"); e != ErrUnsupported {
		t.Fatalf("user-supplied key must be unsupported, got %v", e)
	}
}
