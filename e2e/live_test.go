package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/docs"
)

const (
	pollEvery     = 2 * time.Second
	testBudget    = 8 * time.Minute
	cleanupBudget = 5 * time.Minute
	opBudget      = 3 * time.Minute
	sshReadyWait  = 2 * time.Minute
)

// suite is one test's view of the deployment. Tests never run in parallel.
type suite struct {
	t     *testing.T
	cfg   Config
	c     *controlclient.Client
	hc    *http.Client // plain client for unauthenticated checks; default TLS verification
	n     *namer
	tr    *tracker
	ctx   context.Context
	owner int64
	caps  controlclient.Capabilities
	keys  map[string]string
}

// newSuite skips unless VMAPI_E2E=1. Once enabled, misconfiguration is a hard
// failure (never a silent skip). Only GET requests happen before it returns.
func newSuite(t *testing.T) *suite {
	t.Helper()
	if os.Getenv(envGate) != "1" {
		t.Skip("live E2E disabled; set VMAPI_E2E=1 (see docs/e2e-live.md)")
	}
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		t.Fatalf("live E2E configuration: %v", err)
	}
	hc := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	c, err := controlclient.NewUser(cfg.BaseURL, cfg.Token, cfg.UserID, hc)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	s := &suite{t: t, cfg: cfg, c: c, hc: hc, n: newNamer(), owner: cfg.UserID, keys: map[string]string{}}
	s.tr = &tracker{user: cfg.UserID, base: cfg.BaseImageID, keySeq: s.n.key}
	var cancel context.CancelFunc
	s.ctx, cancel = context.WithTimeout(context.Background(), testBudget)
	t.Cleanup(cancel)
	// Runs at test end (t.Cleanup is LIFO) on
	// its own bounded context, independent of the (maybe expired) test context.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), cleanupBudget)
		defer ccancel()
		for _, e := range s.tr.cleanup(cctx, c, pollEvery) {
			t.Errorf("CLEANUP PROBLEM (resources may remain; ids are in the log): %v", e)
		}
		t.Logf("cleanup done: %d VM(s), %d image(s) tracked", len(s.tr.vms), len(s.tr.images))
	})
	return s
}

// preflight checks capacity and the base image with GET requests only, and
// skips clearly if this run cannot create one VM. No global empty-state is
// assumed: only this user's headroom is evaluated.
func (s *suite) preflight() {
	t := s.t
	t.Helper()
	caps, err := s.c.Capabilities(s.ctx, s.owner)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	s.caps = caps
	vms, err := s.c.ListVMs(s.ctx, s.owner)
	if err != nil {
		t.Fatalf("list VMs: %v", err)
	}
	imgs, err := s.c.Images(s.ctx, s.owner)
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	var vmIDs, imgIDs []string
	for _, v := range vms {
		vmIDs = append(vmIDs, v.ID)
	}
	for _, i := range imgs {
		imgIDs = append(imgIDs, i.ID)
	}
	s.tr.setBaseline(vmIDs, imgIDs) // preexisting resources are never owned or deleted
	if caps.MaxVMsPerUser > 0 && len(vms) >= caps.MaxVMsPerUser {
		t.Skipf("SKIP (capacity): user already has %d/%d VMs", len(vms), caps.MaxVMsPerUser)
	}
	if caps.MaxVMs > 0 && len(vms) >= caps.MaxVMs {
		t.Skipf("SKIP (capacity): API max_vms %d reached by this user", caps.MaxVMs)
	}
	if caps.BackendMaxVMs > 0 && caps.BackendMaxVMs < 1 {
		t.Skip("SKIP (capacity): backend advertises no VM slots")
	}
	if len(caps.Profiles) > 0 && (caps.MaxTotalMemoryMiB > 0 || caps.MaxTotalVCPU > 0) {
		fits := false
		for _, p := range caps.Profiles {
			if (caps.MaxTotalMemoryMiB == 0 || p.MemoryMiB <= caps.MaxTotalMemoryMiB) && (caps.MaxTotalVCPU == 0 || p.VCPU <= caps.MaxTotalVCPU) {
				fits = true
			}
		}
		if !fits {
			t.Skip("SKIP (capacity): no profile fits the advertised aggregate caps")
		}
	}
	img, err := s.c.Image(s.ctx, s.owner, s.cfg.BaseImageID)
	if err != nil {
		t.Fatalf("base image not readable: %v", err)
	}
	if img.Phase != "" && img.Phase != "ready" {
		t.Fatalf("base image phase %q, want ready", img.Phase)
	}
	t.Logf("preflight ok: user has %d VM(s); caps max_vms=%d per_user=%d backend=%d", len(vms), caps.MaxVMs, caps.MaxVMsPerUser, caps.BackendMaxVMs)
}

// opCtx bounds one operation wait.
func (s *suite) wait(opID string) error {
	ctx, cancel := context.WithTimeout(s.ctx, opBudget)
	defer cancel()
	return waitOp(ctx, s.c, s.owner, opID, pollEvery)
}

func (s *suite) mustWait(what, opID string) {
	s.t.Helper()
	if err := s.wait(opID); err != nil {
		s.t.Fatalf("%s operation %s: %v", what, opID, err)
	}
}

// create admits a VM; the ID is registered for cleanup BEFORE waiting.
func (s *suite) create(tag, image string) controlclient.Accepted {
	s.t.Helper()
	a, err := s.c.Create(s.ctx, s.owner, s.n.key("create-"+tag), s.request(tag, image))
	s.trackAccepted(acceptedCreate, a)
	if err != nil {
		s.t.Fatalf("create %s: %v", tag, err)
	}
	if err := s.tr.admitVM(a.VMID); err != nil {
		s.t.Fatalf("create %s: %v", tag, err)
	}
	s.t.Logf("admitted VM %s (op %s) tag=%s", a.VMID, a.OperationID, tag)
	return a
}

// trackUnexpectedVM registers a VM ID returned by a mutation that should not
// have admitted one, so cleanup owns it. Preexisting IDs are not tracked (the
// admit error is only logged; the caller's own assertion fails the test).
func (s *suite) trackUnexpectedVM(id string) {
	s.t.Helper()
	if err := s.tr.admitVM(id); err != nil {
		s.t.Logf("not tracking unexpected admission: %v", err)
	}
}

func (s *suite) createActive(tag, image string) controlclient.Accepted {
	s.t.Helper()
	a := s.create(tag, image)
	s.mustWait("create", a.OperationID)
	vm, err := s.c.VM(s.ctx, s.owner, a.VMID)
	if err != nil || vm.Phase != "active" || vm.OwnerError {
		s.t.Fatalf("VM after create: %v %+v", err, vm)
	}
	return a
}

func (s *suite) deleteVM(tag, id string) {
	s.t.Helper()
	if err := s.tr.mayDeleteVM(id); err != nil {
		s.t.Fatal(err)
	}
	a, err := s.c.Delete(s.ctx, s.owner, id, s.n.key("del-"+tag))
	if err != nil {
		s.t.Fatalf("delete VM %s: %v", id, err)
	}
	s.mustWait("delete VM", a.OperationID)
	s.requireGone(func(ctx context.Context) error { _, e := s.c.VM(ctx, s.owner, id); return e }, "VM "+id)
}

func (s *suite) requireGone(get func(context.Context) error, what string) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	defer cancel()
	if err := waitGone(ctx, time.Second, get); err != nil {
		s.t.Fatalf("%s should be 404: %v", what, err)
	}
}

func (s *suite) commit(tag, vmID string) controlclient.Accepted {
	s.t.Helper()
	a, err := s.c.Commit(s.ctx, s.owner, vmID, s.n.key("commit-"+tag), s.n.name(tag))
	s.trackAccepted(acceptedCommit, a)
	if err != nil {
		s.t.Fatalf("commit %s: %v", tag, err)
	}
	// Track before any assertion; refuses empty, base and preexisting IDs.
	if err := s.tr.admitImage(a.ImageID); err != nil {
		s.t.Fatalf("commit %s: %v", tag, err)
	}
	s.t.Logf("admitted image %s (op %s)", a.ImageID, a.OperationID)
	return a
}

// deleteImage is the only test-side image deletion path; it never touches the
// base image, preexisting images or anything this run did not admit.
func (s *suite) deleteImage(tag, id string) (controlclient.Accepted, error) {
	s.t.Helper()
	if err := s.tr.mayDeleteImage(id); err != nil {
		s.t.Fatal(err)
	}
	return s.c.DeleteImage(s.ctx, s.owner, id, s.n.key(tag))
}

func (s *suite) readyImage(id string) {
	s.t.Helper()
	img, err := s.c.Image(s.ctx, s.owner, id)
	if err != nil || (img.Phase != "" && img.Phase != "ready") || img.ID != id {
		s.t.Fatalf("image %s: %v %+v", id, err, img)
	}
}

// sshReady polls the bounded API readiness (TCP from the API host), then pins
// the host key and proves a real authenticated guest login.
func (s *suite) guestFor(id string) *guest {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, sshReadyWait)
	defer cancel()
	var meta controlclient.SSH
	err := pollUntil(ctx, pollEvery, func(ctx context.Context) (bool, error) {
		m, e := s.c.SSH(ctx, s.owner, id)
		if e != nil {
			return false, e
		}
		meta = m
		return m.Ready, nil
	})
	if err != nil {
		s.t.Fatalf("SSH never reported ready for %s: %v", id, err)
	}
	g, err := pinGuest(ctx, s.t.TempDir(), s.generatedKeyFile(id), s.cfg.KnownHostsFile, meta)
	if err != nil {
		s.t.Fatalf("host key pinning for %s: %v", id, err)
	}
	if err := pollUntil(ctx, pollEvery, func(ctx context.Context) (bool, error) {
		out, e := g.exec(ctx, "echo vmapi-e2e-ok")
		return strings.TrimSpace(out) == "vmapi-e2e-ok", e
	}); err != nil {
		s.t.Fatalf("guest SSH login for %s: %v", id, err)
	}
	return g
}

func (s *suite) writeMarker(g *guest, tag string) string {
	s.t.Helper()
	marker := "vmapie2e-" + randHex(12)
	if _, err := g.exec(s.ctx, writeMarkerCmd(markerPath(s.n.run, tag), marker)); err != nil {
		s.t.Fatalf("write guest marker: %v", err)
	}
	s.readMarker(g, tag, marker)
	return marker
}

func (s *suite) readMarker(g *guest, tag, want string) {
	s.t.Helper()
	out, err := g.exec(s.ctx, readMarkerCmd(markerPath(s.n.run, tag)))
	if err != nil || out != want {
		s.t.Fatalf("guest marker mismatch (err=%v, got %d bytes, want %d)", err, len(out), len(want))
	}
}

func (s *suite) stop(id string) {
	s.t.Helper()
	a, err := s.c.Stop(s.ctx, s.owner, id, s.n.key("stop"))
	if err != nil {
		s.t.Fatalf("stop: %v", err)
	}
	s.mustWait("stop", a.OperationID)
	vm, err := s.c.VM(s.ctx, s.owner, id)
	if err != nil || vm.Phase != "stopped" {
		s.t.Fatalf("after stop: %v %+v", err, vm)
	}
}

// ---------------------------------------------------------------- tests

func (s *suite) get(path string, hdr map[string]string) (int, http.Header, []byte) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.cfg.BaseURL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err) // includes TLS verification failures
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, resp.Header, b
}

// TestPublicDocsHealthAndAuth uses only unauthenticated or invalid requests and
// read-only GETs: nothing is created.
func TestPublicDocsHealthAndAuth(t *testing.T) {
	s := newSuite(t)

	code, _, body := s.get("/healthz", nil)
	var h struct{ Status string }
	if code != 200 || json.Unmarshal(body, &h) != nil || h.Status != "ok" {
		t.Fatalf("/healthz: %d", code)
	}

	code, hdr, body := s.get("/docs", nil)
	page := string(body)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") || hdr.Get("Content-Security-Policy") == "" {
		t.Fatalf("/docs: %d %q", code, hdr.Get("Content-Type"))
	}
	for _, want := range []string{"/docs/assets/swagger-ui-bundle.js", "/docs/assets/swagger-ui.css", "/docs/assets/swagger-initializer.js"} {
		if !strings.Contains(page, want) {
			t.Errorf("/docs lacks local asset %s", want)
		}
	}
	if strings.Contains(page, "http://") || strings.Contains(page, "https://") {
		t.Error("/docs references an absolute (external) URL")
	}
	for _, a := range []struct {
		path, ctype string
		min         int
	}{
		{"/docs/assets/swagger-ui.css", "text/css", 1000},
		{"/docs/assets/swagger-ui-bundle.js", "javascript", 100000},
		{"/docs/assets/swagger-initializer.js", "javascript", 20},
		{"/docs/assets/favicon-32x32.png", "image/png", 100},
	} {
		code, hdr, b := s.get(a.path, nil)
		if code != 200 || !strings.Contains(hdr.Get("Content-Type"), a.ctype) || len(b) < a.min {
			t.Errorf("%s: %d %q %d bytes", a.path, code, hdr.Get("Content-Type"), len(b))
		}
		if a.path == "/docs/assets/swagger-initializer.js" && !bytes.Contains(b, []byte("/openapi.yaml")) {
			t.Error("initializer does not point at /openapi.yaml")
		}
	}
	if code, _, _ := s.get("/docs/assets/not-an-asset.js", nil); code == 200 {
		t.Error("unknown asset path served")
	}

	// OpenAPI schemas must match this checkout except the configured CreateVM
	// example; executing that exact example is a separate authenticated test.
	code, _, live := s.get("/openapi.yaml", nil)
	if code != 200 {
		t.Fatalf("/openapi.yaml: %d", code)
	}
	var liveDoc, srcDoc map[interface{}]interface{}
	if err := yaml.Unmarshal(live, &liveDoc); err != nil {
		t.Fatalf("live OpenAPI does not parse: %v", err)
	}
	if err := yaml.Unmarshal(docs.OpenAPI, &srcDoc); err != nil {
		t.Fatalf("source OpenAPI does not parse: %v", err)
	}
	paths, _ := liveDoc["paths"].(map[interface{}]interface{})
	if _, ok := paths["/v1/vms"]; !ok || liveDoc["openapi"] == nil {
		t.Error("live OpenAPI lacks openapi version or /v1/vms")
	}
	removeCreateExample(liveDoc)
	removeCreateExample(srcDoc)
	if !reflect.DeepEqual(liveDoc, srcDoc) {
		t.Error("live OpenAPI parses differently from docs/control-api-openapi.yaml (revision mismatch?)")
	}

	// Snapshot baseline before the negative mutation too; unexpected admissions
	// must be tracked without ever owning preexisting resources.
	baselineVMs, err := s.c.ListVMs(s.ctx, s.owner)
	if err != nil {
		t.Fatal(err)
	}
	baselineImages, err := s.c.Images(s.ctx, s.owner)
	if err != nil {
		t.Fatal(err)
	}
	var baselineVMIDs, baselineImageIDs []string
	for _, v := range baselineVMs {
		baselineVMIDs = append(baselineVMIDs, v.ID)
	}
	for _, i := range baselineImages {
		baselineImageIDs = append(baselineImageIDs, i.ID)
	}
	s.tr.setBaseline(baselineVMIDs, baselineImageIDs)
	// Unauthenticated / invalid credentials are rejected. The POST uses a
	// nonexistent image so it can never admit work even if auth were broken.
	for _, p := range []string{"/v1/vms", "/v1/images", "/v1/capabilities"} {
		if code, _, _ := s.get(p, nil); code != 401 {
			t.Errorf("unauthenticated GET %s: %d, want 401", p, code)
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", s.cfg.BaseURL+"/v1/vms", strings.NewReader(`{"image_id":"e2e-no-such-image"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", s.n.key("unauth"))
	if resp, err := s.hc.Do(req); err != nil {
		t.Errorf("unauthenticated POST: %v", err)
	} else {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		var unexpected controlclient.Accepted
		if json.Unmarshal(body, &unexpected) == nil {
			s.trackAccepted(acceptedCreate, unexpected)
		}
		if resp.StatusCode != 401 {
			t.Errorf("unauthenticated POST /v1/vms: %d, want 401", resp.StatusCode)
		}
	}
	bad, err := controlclient.NewUser(s.cfg.BaseURL, "e2e-invalid-bearer", s.owner, s.hc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.ListVMs(s.ctx, s.owner); !isStatus(err, 401) {
		t.Errorf("invalid bearer: %v, want 401", err)
	}
	if _, err := s.c.ListVMs(s.ctx, s.owner); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
}

// TestLifecycleStopCommitsAndConflicts: create + idempotency, SSH marker,
// stop, terminal-on-stopped 409, two cold commits, source deletion preserves
// both images.
func TestLifecycleStopCommitsAndConflicts(t *testing.T) {
	s := newSuite(t)
	s.preflight()

	key := s.n.key("idem")
	req := s.request("life", s.cfg.BaseImageID)
	a, err := s.c.Create(s.ctx, s.owner, key, req)
	s.trackAccepted(acceptedCreate, a)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.tr.admitVM(a.VMID); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("admitted VM %s (op %s)", a.VMID, a.OperationID)
	dup, err := s.c.Create(s.ctx, s.owner, key, req)
	s.trackAccepted(acceptedCreate, dup)
	if err == nil {
		s.trackUnexpectedVM(dup.VMID) // before asserting: a wrong replay may admit another VM
	}
	if err != nil || dup.VMID != a.VMID || dup.OperationID != a.OperationID {
		t.Fatalf("idempotent replay: %v %+v", err, dup)
	}
	changed := req
	changed.Name = s.n.name("life2")
	bad, err := s.c.Create(s.ctx, s.owner, key, changed)
	s.trackAccepted(acceptedCreate, bad)
	if err == nil {
		s.trackUnexpectedVM(bad.VMID) // before asserting: success here admitted a VM
	}
	if !isStatus(err, 409) {
		t.Fatalf("same key, changed body: %v, want 409", err)
	}
	s.mustWait("create", a.OperationID)

	g := s.guestFor(a.VMID)
	beforeKey, e := s.c.SSHKey(s.ctx, s.owner, a.VMID)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := s.c.Create(s.ctx, s.owner, key, req)
	s.trackAccepted(acceptedCreate, replay)
	if e != nil || replay != a {
		t.Fatal("post-retrieval replay changed admission")
	}
	afterKey, e := s.c.SSHKey(s.ctx, s.owner, a.VMID)
	if e != nil || beforeKey != afterKey {
		t.Fatal("Create replay regenerated credentials")
	}
	for _, path := range []string{"/v1/vms", "/v1/vms/" + a.VMID, "/v1/capabilities", "/v1/operations/" + a.OperationID} {
		code, _, body := s.get(path, map[string]string{"Authorization": "Bearer " + s.cfg.Token})
		if code != 200 || bytes.Contains(body, []byte("BEGIN OPENSSH PRIVATE KEY")) {
			t.Fatal("ordinary API response contains private key or failed")
		}
	}
	marker := s.writeMarker(g, "life")
	s.readMarker(g, "life", marker)

	s.stop(a.VMID)
	if m, err := s.c.SSH(s.ctx, s.owner, a.VMID); err != nil || m.Ready {
		t.Fatalf("SSH readiness on stopped VM: %v ready=%v", err, m.Ready)
	}
	termAcc, termErr := s.c.Terminal(s.ctx, s.owner, a.VMID, s.n.key("term"))
	s.trackAccepted(acceptedReferences, termAcc)
	if err := termErr; !isStatus(err, 409) {
		t.Fatalf("terminal on stopped VM: %v, want 409", err)
	}

	// Serial: a second commit admitted while the first is pending correctly
	// conflicts, so finish the first before submitting the second.
	c1 := s.commit("cold1", a.VMID)
	s.mustWait("commit 1", c1.OperationID)
	c2 := s.commit("cold2", a.VMID)
	s.mustWait("commit 2", c2.OperationID)
	if c1.ImageID == c2.ImageID {
		t.Fatal("two commits returned the same image")
	}
	s.readyImage(c1.ImageID)
	s.readyImage(c2.ImageID)

	s.deleteVM("src", a.VMID)
	s.readyImage(c1.ImageID)
	s.readyImage(c2.ImageID)
	if _, err := s.c.Image(s.ctx, s.owner, s.cfg.BaseImageID); err != nil {
		t.Fatalf("base image disturbed: %v", err)
	}
}

// TestDerivativeFromCommittedImage: marker survives commit -> new VM; image in
// use cannot be deleted; deleting is fine once the derivative is gone.
func TestDerivativeFromCommittedImage(t *testing.T) {
	s := newSuite(t)
	s.preflight()

	src := s.createActive("dsrc", s.cfg.BaseImageID)
	marker := s.writeMarker(s.guestFor(src.VMID), "deriv")
	s.stop(src.VMID)
	cm := s.commit("dimg", src.VMID)
	s.mustWait("commit", cm.OperationID)
	s.readyImage(cm.ImageID)
	s.deleteVM("dsrc", src.VMID) // frees the slot before the derivative boots

	d := s.createActive("dder", cm.ImageID)
	s.readMarker(s.guestFor(d.VMID), "deriv", marker)

	if _, err := s.deleteImage("imgdel-busy", cm.ImageID); !isStatus(err, 409) {
		t.Fatalf("delete of in-use image: %v, want 409", err)
	}
	s.readyImage(cm.ImageID)
	s.deleteVM("dder", d.VMID)
	di, err := s.deleteImage("imgdel", cm.ImageID)
	if err != nil {
		t.Fatalf("delete image after VM gone: %v", err)
	}
	s.mustWait("delete image", di.OperationID)
	s.requireGone(func(ctx context.Context) error { _, e := s.c.Image(ctx, s.owner, cm.ImageID); return e }, "image "+cm.ImageID)
}

// TestEarlyDeleteAfterCreate queues Delete while the guest is still being created.
func TestEarlyDeleteAfterCreate(t *testing.T) {
	s := newSuite(t)
	s.preflight()

	a := s.create("early", s.cfg.BaseImageID)
	d, err := s.c.Delete(s.ctx, s.owner, a.VMID, s.n.key("early-del"))
	if err != nil {
		t.Fatalf("early delete: %v", err)
	}
	// Sequential owner contract: the queued Delete waits for Create, so both
	// operations must succeed and the VM must then be gone.
	s.mustWait("create", a.OperationID)
	s.mustWait("delete", d.OperationID)
	s.requireGone(func(ctx context.Context) error { _, e := s.c.VM(ctx, s.owner, a.VMID); return e }, "VM "+a.VMID)
}

// TestOptionalAPIRestart runs only with VMAPI_E2E_RESTART_USER_UNIT. Evidence
// is API/SSH-visible continuity (same VM ID/SSH endpoint/host key, same guest
// boot_id and marker). It does NOT prove the VMM process was untouched.
func TestOptionalAPIRestart(t *testing.T) {
	if os.Getenv(envGate) == "1" && strings.TrimSpace(os.Getenv(envRestartU)) == "" {
		t.Skipf("restart test needs explicit %s", envRestartU)
	}
	s := newSuite(t)
	s.preflight()

	a := s.createActive("rst", s.cfg.BaseImageID)
	g := s.guestFor(a.VMID)
	marker := s.writeMarker(g, "rst")
	boot := func() string {
		out, err := g.exec(s.ctx, "cat /proc/sys/kernel/random/boot_id")
		if err != nil || strings.TrimSpace(out) == "" {
			t.Fatalf("boot_id: %v", err)
		}
		return strings.TrimSpace(out)
	}
	bootBefore := boot()
	vmBefore, err := s.c.VM(s.ctx, s.owner, a.VMID)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, keyErr := s.c.SSHKey(s.ctx, s.owner, a.VMID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	sshBefore, err := s.c.SSH(s.ctx, s.owner, a.VMID)
	if err != nil {
		t.Fatal(err)
	}

	rctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(rctx, "systemctl", "--user", "restart", s.cfg.RestartUnit) // no shell
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("systemctl --user restart %s: %v: %.300s", s.cfg.RestartUnit, err, out)
	}
	hctx, hcancel := context.WithTimeout(s.ctx, 90*time.Second)
	defer hcancel()
	if err := pollUntil(hctx, time.Second, func(ctx context.Context) (bool, error) {
		r, _ := http.NewRequestWithContext(ctx, "GET", s.cfg.BaseURL+"/healthz", nil)
		resp, e := s.hc.Do(r)
		if e != nil {
			return false, e
		}
		resp.Body.Close()
		return resp.StatusCode == 200, fmt.Errorf("healthz %d", resp.StatusCode)
	}); err != nil {
		t.Fatalf("API not healthy after restart: %v", err)
	}

	vmAfter, err := s.c.VM(s.ctx, s.owner, a.VMID)
	if err != nil || !reflect.DeepEqual(vmAfter, vmBefore) {
		t.Fatalf("VM changed across restart: %v", err)
	}
	keyAfter, keyErr := s.c.SSHKey(s.ctx, s.owner, a.VMID)
	if keyErr != nil || keyBefore != keyAfter {
		t.Fatal("generated key changed across API restart")
	}
	sshAfter, err := s.c.SSH(s.ctx, s.owner, a.VMID)
	if err != nil || sshAfter.Host != sshBefore.Host || sshAfter.Port != sshBefore.Port || sshAfter.HostKeyFingerprint != sshBefore.HostKeyFingerprint {
		t.Fatalf("SSH identity changed across restart: %v", err)
	}
	s.readMarker(g, "rst", marker)
	if after := boot(); after != bootBefore {
		t.Fatal("guest boot_id changed: guest was recreated or rebooted")
	}
}
