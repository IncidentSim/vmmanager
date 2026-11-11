package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	key := filepath.Join(dir, "key")
	os.WriteFile(tok, []byte("secret-token-value\n"), 0o600)
	os.WriteFile(key, []byte("k"), 0o600)
	good := map[string]string{envBaseURL: "https://host.example:1/", envUserID: "7", envTokenF: tok, envImage: "ubuntu", envSSHKeyF: key, envDirect: "1"}
	cp := func(over map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	c, err := loadConfig(env(good))
	if err != nil || c.Token != "secret-token-value" || c.BaseURL != "https://host.example:1" || c.UserID != 7 || c.RestartUnit != "" {
		t.Fatalf("good: %v %+v", err, c)
	}
	loose := filepath.Join(dir, "loose")
	os.WriteFile(loose, []byte("k"), 0o644)
	for name, over := range map[string]map[string]string{
		"http":      {envBaseURL: "http://host.example"},
		"path":      {envBaseURL: "https://host.example/x"},
		"userinfo":  {envBaseURL: "https://u:p@host.example"},
		"uid":       {envUserID: "0"},
		"image":     {envImage: "Bad Image"},
		"unit":      {envRestartU: "x; reboot.service"},
		"unit2":     {envRestartU: "plain"},
		"nodirect":  {envDirect: ""},
		"looskey":   {envSSHKeyF: loose},
		"missingtk": {envTokenF: filepath.Join(dir, "none")},
		"nogate":    {envBaseURL: ""},
	} {
		_, err := loadConfig(env(cp(over)))
		if err == nil {
			t.Errorf("%s: expected error", name)
		} else if strings.Contains(err.Error(), "secret-token-value") {
			t.Errorf("%s: error leaks token", name)
		}
	}
	if c, err := loadConfig(env(cp(map[string]string{envRestartU: "firecracker-vmapi.service"}))); err != nil || c.RestartUnit == "" {
		t.Fatalf("unit: %v", err)
	}
	// Optional known_hosts file.
	khf := filepath.Join(dir, "kh")
	os.WriteFile(khf, []byte("SECRET-HOSTKEY-LINE\n"), 0o644)
	if c, err := loadConfig(env(cp(map[string]string{envKnownF: khf}))); err != nil || c.KnownHostsFile != khf {
		t.Fatalf("known hosts: %v %+v", err, c)
	}
	if c, _ := loadConfig(env(good)); c.KnownHostsFile != "" {
		t.Fatal("known hosts should default empty")
	}
	for name, p := range map[string]string{"missing": filepath.Join(dir, "nokh"), "dir": dir} {
		_, err := loadConfig(env(cp(map[string]string{envKnownF: p})))
		if err == nil {
			t.Errorf("known hosts %s accepted", name)
		} else if strings.Contains(err.Error(), p) || strings.Contains(err.Error(), "SECRET-HOSTKEY") {
			t.Errorf("known hosts %s: error leaks value", name)
		}
	}
	os.WriteFile(tok, []byte("two words"), 0o600)
	if _, err := loadConfig(env(good)); err == nil {
		t.Fatal("token with whitespace accepted")
	}
}

func TestNamerValid(t *testing.T) {
	n := newNamer()
	for _, tag := range []string{"life", "derive", "early", "restart", "commit1", "123456789"} {
		nm := n.name(tag)
		if len(nm) > 20 || !labelPattern.MatchString(nm) || strings.HasPrefix(nm, "bot-") {
			t.Errorf("invalid name %q", nm)
		}
	}
	a, b := n.key("x"), n.key("x")
	if a == b || len(a) > 200 {
		t.Fatalf("keys %q %q", a, b)
	}
	for _, r := range a {
		if r < 33 || r > 126 {
			t.Fatal("non-printable key")
		}
	}
}

func TestPollUntil(t *testing.T) {
	n := 0
	if err := pollUntil(context.Background(), time.Millisecond, func(context.Context) (bool, error) { n++; return n == 3, errors.New("x") }); err != nil || n != 3 {
		t.Fatal(err, n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := pollUntil(ctx, 5*time.Millisecond, func(context.Context) (bool, error) { return false, errors.New("last-err") })
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "last-err") {
		t.Fatal(err)
	}
	perm := errors.New("perm")
	if err := pollUntil(context.Background(), time.Millisecond, func(context.Context) (bool, error) { return false, permanent(perm) }); err != perm {
		t.Fatal(err)
	}
}

func TestSSHHelpers(t *testing.T) {
	if validEndpoint("10.0.0.2", 22, "root") != nil || validEndpoint("-oProxyCommand=x", 22, "root") == nil ||
		validEndpoint("10.0.0.2", 0, "root") == nil || validEndpoint("10.0.0.2", 22, "-oBad") == nil {
		t.Fatal("endpoint validation")
	}
	if fingerprintFromKeygen("256 SHA256:abc+/ no comment (ED25519)") != "SHA256:abc+/" || fingerprintFromKeygen("junk") != "" {
		t.Fatal("fingerprint parse")
	}
	l, ok := knownHostsLine("10.0.0.2", 2222, "10.0.0.2 ssh-ed25519 AAAA")
	if !ok || l != "[10.0.0.2]:2222 ssh-ed25519 AAAA" {
		t.Fatal(l)
	}
	if l, _ := knownHostsLine("10.0.0.2", 22, "x ssh-ed25519 AAAA"); l != "10.0.0.2 ssh-ed25519 AAAA" {
		t.Fatal(l)
	}
	if _, ok := knownHostsLine("h", 22, "# comment"); ok {
		t.Fatal("comment accepted")
	}
	// Operator known_hosts: used verbatim, strict, no fingerprint needed.
	pg, err := pinGuest(context.Background(), t.TempDir(), "/k", "/trusted/kh", controlclient.SSH{Host: "10.0.0.2", Port: 22, User: "root"})
	if err != nil || pg.known != "/trusted/kh" {
		t.Fatalf("operator known_hosts: %v %+v", err, pg)
	}
	pa := strings.Join(pg.sshArgs("true"), " ")
	if !strings.Contains(pa, "StrictHostKeyChecking=yes") || !strings.Contains(pa, "UserKnownHostsFile=/trusted/kh") || !strings.Contains(pa, "GlobalKnownHostsFile=/dev/null") {
		t.Fatal(pa)
	}
	if _, err := pinGuest(context.Background(), t.TempDir(), "/k", "/trusted/kh", controlclient.SSH{Host: "-oProxyCommand=x", Port: 22, User: "root"}); err == nil {
		t.Fatal("bad endpoint accepted with known_hosts")
	}
	// Without the file, an empty API fingerprint still fails closed.
	if _, err := pinGuest(context.Background(), t.TempDir(), "/k", "", controlclient.SSH{Host: "10.0.0.2", Port: 22, User: "root"}); err == nil {
		t.Fatal("empty fingerprint accepted without known_hosts file")
	}
	g := &guest{host: "10.0.0.2", port: 22, user: "root", key: "/k", known: "/kh"}
	args := strings.Join(g.sshArgs("true"), " ")
	if !strings.Contains(args, "StrictHostKeyChecking=yes") || strings.Contains(args, "StrictHostKeyChecking=no") || strings.Contains(args, "UserKnownHostsFile=/dev/null") {
		t.Fatal(args)
	}
}

// fakeAPI models a tiny backend for cleanup tests.
type fakeAPI struct {
	mu        sync.Mutex
	vms       map[string]bool
	images    map[string]int // image -> remaining 409 answers
	deleted   []string
	ops       map[string]string
	failVMDel bool
}

func (f *fakeAPI) VM(_ context.Context, _ int64, id string) (controlclient.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.vms[id] {
		return controlclient.VM{}, &controlclient.APIError{Code: "not_found", Status: 404}
	}
	return controlclient.VM{ID: id}, nil
}
func (f *fakeAPI) Delete(_ context.Context, _ int64, id, _ string) (controlclient.Accepted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failVMDel {
		return controlclient.Accepted{}, &controlclient.APIError{Code: "internal", Status: 500}
	}
	delete(f.vms, id)
	f.deleted = append(f.deleted, "vm:"+id)
	return controlclient.Accepted{OperationID: "op-" + id}, nil
}
func (f *fakeAPI) DeleteImage(_ context.Context, _ int64, id, _ string) (controlclient.Accepted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.images[id]
	if !ok {
		return controlclient.Accepted{}, &controlclient.APIError{Code: "not_found", Status: 404}
	}
	if n > 0 {
		f.images[id] = n - 1
		return controlclient.Accepted{}, &controlclient.APIError{Code: "conflict", Status: 409}
	}
	delete(f.images, id)
	f.deleted = append(f.deleted, "img:"+id)
	return controlclient.Accepted{OperationID: "op-" + id}, nil
}
func (f *fakeAPI) Image(_ context.Context, _ int64, id string) (controlclient.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.images[id]; !ok {
		return controlclient.Image{}, &controlclient.APIError{Code: "not_found", Status: 404}
	}
	return controlclient.Image{ID: id}, nil
}
func (f *fakeAPI) Operation(_ context.Context, _ int64, id string) (controlclient.Operation, error) {
	return controlclient.Operation{ID: id, Status: "succeeded"}, nil
}

func TestTrackerCleanupOnlyOwnResources(t *testing.T) {
	f := &fakeAPI{vms: map[string]bool{"mine": true, "theirs": true}, images: map[string]int{"mine-img": 2, "ubuntu": 0, "theirs-img": 0}}
	n := newNamer()
	tr := &tracker{user: 1, base: "ubuntu", keySeq: n.key}
	tr.addVM("mine")
	tr.addVM("already-gone")
	tr.addImage("mine-img")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if errs := tr.cleanup(ctx, f, time.Millisecond); len(errs) != 0 {
		t.Fatal(errs)
	}
	if !f.vms["theirs"] {
		t.Fatal("foreign VM deleted")
	}
	if _, ok := f.images["ubuntu"]; !ok {
		t.Fatal("base image deleted")
	}
	if _, ok := f.images["theirs-img"]; !ok {
		t.Fatal("foreign image deleted")
	}
	if strings.Join(f.deleted, ",") != "vm:mine,img:mine-img" {
		t.Fatal(f.deleted)
	}
}

func TestTrackerRefusesBaseAndReportsErrors(t *testing.T) {
	f := &fakeAPI{vms: map[string]bool{"v": true}, images: map[string]int{"ubuntu": 0}, failVMDel: true}
	tr := &tracker{user: 1, base: "ubuntu", keySeq: newNamer().key}
	tr.addVM("v")
	tr.addImage("ubuntu")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errs := tr.cleanup(ctx, f, time.Millisecond)
	if len(errs) != 2 {
		t.Fatalf("want 2 errors, got %v", errs)
	}
	if _, ok := f.images["ubuntu"]; !ok {
		t.Fatal("base image deleted")
	}
}

func TestWaitOpFailure(t *testing.T) {
	err := waitOp(context.Background(), &failingOps{}, 1, "x", time.Millisecond)
	var a *controlclient.APIError
	if !errors.As(err, &a) || a.Code != "capacity" {
		t.Fatal(err)
	}
}

type failingOps struct{ fakeAPI }

func (*failingOps) Operation(_ context.Context, _ int64, id string) (controlclient.Operation, error) {
	return controlclient.Operation{ID: id, Status: "failed", Error: &controlclient.APIError{Code: "capacity"}}, nil
}

func TestTrackerAdmissionGuards(t *testing.T) {
	tr := &tracker{user: 1, base: "ubuntu", keySeq: newNamer().key}
	tr.setBaseline([]string{"pre-vm"}, []string{"pre-img", "ubuntu"})
	for _, id := range []string{"", "pre-vm"} {
		if tr.admitVM(id) == nil {
			t.Errorf("admitVM(%q) accepted", id)
		}
	}
	for _, id := range []string{"", "ubuntu", "pre-img"} {
		if tr.admitImage(id) == nil {
			t.Errorf("admitImage(%q) accepted", id)
		}
	}
	if len(tr.vms) != 0 || len(tr.images) != 0 {
		t.Fatalf("refused IDs were tracked: %v %v", tr.vms, tr.images)
	}
	if tr.admitVM("new-vm") != nil || tr.admitVM("new-vm") != nil || tr.admitImage("new-img") != nil || tr.admitImage("new-img") != nil {
		t.Fatal("new IDs refused")
	}
	if len(tr.vms) != 1 || len(tr.images) != 1 {
		t.Fatalf("duplicate admission tracked twice: %v %v", tr.vms, tr.images)
	}
	for _, id := range []string{"ubuntu", "pre-img", "untracked"} {
		if tr.mayDeleteImage(id) == nil {
			t.Errorf("mayDeleteImage(%q) allowed", id)
		}
	}
	if err := tr.mayDeleteImage("new-img"); err != nil {
		t.Fatal(err)
	}
}

// A buggy API that hands back preexisting resources must not make cleanup
// delete them, while a genuinely new unexpected VM is still cleaned up.
func TestCleanupSkipsPreexistingButOwnsUnexpectedAdmission(t *testing.T) {
	f := &fakeAPI{vms: map[string]bool{"pre-vm": true, "stray": true}, images: map[string]int{"pre-img": 0, "ubuntu": 0}}
	tr := &tracker{user: 1, base: "ubuntu", keySeq: newNamer().key}
	tr.setBaseline([]string{"pre-vm"}, []string{"pre-img"})
	_ = tr.admitVM("pre-vm") // replay returned an existing VM: refused
	if err := tr.admitVM("stray"); err != nil {
		t.Fatal(err)
	}
	// Even if something bypassed admission, cleanup itself refuses.
	tr.addVM("pre-vm")
	tr.addImage("pre-img")
	tr.addImage("ubuntu")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := tr.cleanup(ctx, f, time.Millisecond)
	if len(errs) != 3 {
		t.Fatalf("want 3 refusals, got %v", errs)
	}
	if !f.vms["pre-vm"] || f.vms["stray"] {
		t.Fatalf("vms: %v", f.vms)
	}
	if _, ok := f.images["pre-img"]; !ok {
		t.Fatal("preexisting image deleted")
	}
	if _, ok := f.images["ubuntu"]; !ok {
		t.Fatal("base image deleted")
	}
	if strings.Join(f.deleted, ",") != "vm:stray" {
		t.Fatal(f.deleted)
	}
}

// The lifecycle test must finish commit 1 before admitting commit 2, and must
// track unexpected create admissions before asserting on them.
func TestLifecycleSourceStructure(t *testing.T) {
	b, err := os.ReadFile("live_test.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	at := func(sub string) int {
		i := strings.Index(src, sub)
		if i < 0 {
			t.Fatalf("missing %q", sub)
		}
		return i
	}
	if !(at(`s.commit("cold1"`) < at(`s.mustWait("commit 1"`) && at(`s.mustWait("commit 1"`) < at(`s.commit("cold2"`)) {
		t.Error("commit 1 must be awaited before commit 2 is submitted")
	}
	if at("s.trackUnexpectedVM(dup.VMID)") > at("idempotent replay:") || at("s.trackUnexpectedVM(bad.VMID)") > at("same key, changed body:") {
		t.Error("unexpected admissions must be tracked before assertions")
	}
	if strings.Count(src, ".DeleteImage(") != 1 {
		t.Error("image deletion must go through suite.deleteImage only")
	}
}
