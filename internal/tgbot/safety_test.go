package tgbot

import (
	"context"
	"encoding/json"
	"github.com/petruekhin/vibe-firecracker/controlclient"
	"golang.org/x/sys/unix"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUncertainFailureNeverClaimsNoChange(t *testing.T) {
	b, tg, _ := testBot(t)
	b.fail(context.Background(), 42, &controlclient.APIError{Code: "unavailable", Status: 503})
	if strings.Contains(tg.messages[0].Text, "не изменено") {
		t.Fatal("transport/server failure is not proof of no durable mutation")
	}
}
func TestNoUnsafeTerminalURLOrRawServerError(t *testing.T) {
	b, tg, _ := testBot(t)
	for _, link := range []string{"http://external/t/cap/", "javascript:alert(1)", "https://user:password@example.com/t/cap/"} {
		raw, _ := json.Marshal(map[string]string{"url": link})
		before := len(tg.messages)
		b.showOperation(context.Background(), 42, controlclient.Operation{ID: "op", Status: "succeeded", Result: raw})
		if len(tg.messages) != before+1 || strings.Contains(tg.messages[before].Text, link) {
			t.Fatal("unsafe terminal URL sent")
		}
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"error":{"code":"unavailable","message":"token SECRET /private/path"}}`))
	}))
	defer s.Close()
	b.client, _ = controlclient.New(s.URL, "service-secret", nil)
	b.Handle(context.Background(), message(3, 42, "/list"))
	m := tg.messages[len(tg.messages)-1]
	if strings.Contains(m.Text, "SECRET") || strings.Contains(m.Text, "/private") {
		t.Fatal("backend secrets displayed")
	}
}
func TestImportV1StoppedLockAndPermission(t *testing.T) {
	d := privateDir(t)
	s, e := OpenDelivery(d)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	old := privateDir(t)
	source := filepath.Join(old, "state.json")
	raw := []byte(`{"version":1,"offset":1234,"vm":{"token":"legacy-secret","generation":3}}`)
	os.WriteFile(source, raw, 0600)
	lock := filepath.Join(old, "lock")
	os.WriteFile(lock, nil, 0600)
	f, e := os.Open(lock)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		t.Fatal(e)
	}
	if e = s.ImportLegacy(source, true); e == nil {
		t.Fatal("running legacy lock ignored")
	}
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	os.Chmod(source, 0644)
	if e = s.ImportLegacy(source, true); e == nil {
		t.Fatal("public legacy state accepted")
	}
	os.Chmod(source, 0600)
	link := filepath.Join(old, "link")
	os.Symlink(source, link)
	if e = s.ImportLegacy(link, true); e == nil {
		t.Fatal("symlink source accepted")
	}
	if e = s.ImportLegacy(source, true); e != nil {
		t.Fatal(e)
	}
	if s.Offset() != 1234 {
		t.Fatal(s.Offset())
	}
	persisted, _ := os.ReadFile(filepath.Join(d, "delivery.json"))
	if strings.Contains(string(persisted), "legacy-secret") {
		t.Fatal("VM secrets copied into delivery state")
	}
}
func TestOffsetWriteFailurePreventsCommand(t *testing.T) {
	b, tg, f := testBot(t)
	s, e := OpenDelivery(privateDir(t))
	if e != nil {
		t.Fatal(e)
	}
	b.Delivery = s
	s.Close()
	tg.updates = []Update{message(1, 42, "/create")}
	if e = b.Run(context.Background()); e == nil {
		t.Fatal("offset write failure ignored")
	}
	if f.calls != 0 || len(tg.messages) != 0 {
		t.Fatal("command processed without durable offset")
	}
}
