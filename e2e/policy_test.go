package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v2"
)

func removeCreateExample(doc map[interface{}]interface{}) {
	if c, ok := doc["components"].(map[interface{}]interface{}); ok {
		if schemas, ok := c["schemas"].(map[interface{}]interface{}); ok {
			if create, ok := schemas["CreateVM"].(map[interface{}]interface{}); ok {
				delete(create, "example")
			}
		}
	}
}

func policyTTL(c controlclient.Capabilities, desired int64) (int64, error) {
	if c.TTL == nil || c.TTL.MaxSeconds < 1 {
		return 0, errors.New("finite TTL policy not advertised")
	}
	if desired > c.TTL.MaxSeconds {
		desired = c.TTL.MaxSeconds
	}
	return desired, nil
}

func (s *suite) request(tag, image string) controlclient.CreateRequest {
	ttl, err := policyTTL(s.caps, 600)
	if err != nil {
		s.t.Fatal(err)
	}
	if s.caps.DefaultProfile == "" {
		s.t.Fatal("default profile not advertised")
	}
	return controlclient.CreateRequest{Name: s.n.name(tag), ImageID: image, Profile: s.caps.DefaultProfile, TTLSeconds: ttl}
}

func (s *suite) admitRequest(r controlclient.CreateRequest) controlclient.Accepted {
	s.t.Helper()
	a, err := s.c.Create(s.ctx, s.owner, s.n.key("policy"), r)
	s.trackAccepted(acceptedCreate, a)
	if err != nil {
		s.t.Fatalf("create policy VM: %v", err)
	}
	if a.VMID == "" {
		s.t.Fatal("empty admitted VM")
	}
	return a
}

type acceptedIntent uint8

const (
	acceptedReferences acceptedIntent = iota // Stop, Terminal, Delete: no new resource.
	acceptedCreate                           // VMID is new; ImageID is the source.
	acceptedCommit                           // ImageID is new; VMID is the source.
)

// Intent comes from the request, never from which response fields are present.
// Empty envelopes are harmless on rejected requests; callers validate required
// IDs on success. Track partial/unexpected admissions before any assertion.
func (tr *tracker) trackAccepted(intent acceptedIntent, a controlclient.Accepted) error {
	switch intent {
	case acceptedCreate:
		if a.VMID != "" {
			return tr.admitVM(a.VMID)
		}
	case acceptedCommit:
		if a.ImageID != "" {
			return tr.admitImage(a.ImageID)
		}
	case acceptedReferences:
	default:
		return errors.New("unknown accepted operation intent")
	}
	return nil
}

func (s *suite) trackAccepted(intent acceptedIntent, a controlclient.Accepted) {
	s.t.Helper()
	if err := s.tr.trackAccepted(intent, a); err != nil {
		s.t.Fatal(err)
	}
}

// No private key or SSH output is printed, including on parse failures.
func (s *suite) generatedKeyFile(id string) string {
	if p := s.keys[id]; p != "" {
		return p
	}
	k, err := s.c.SSHKey(s.ctx, s.owner, id)
	if err != nil {
		s.t.Fatalf("generated key retrieval: %v", err)
	}
	k2, err := s.c.SSHKey(s.ctx, s.owner, id)
	if err != nil || k.PrivateKey == "" || k != k2 || k.VMID != id {
		s.t.Fatal("generated key retrieval is empty, unstable or bound to wrong VM")
	}
	signer, err := ssh.ParsePrivateKey([]byte(k.PrivateKey))
	if err != nil || strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != strings.TrimSpace(k.PublicKey) {
		s.t.Fatal("invalid generated key pair")
	}
	code, h, b := s.get("/v1/vms/"+id+"/ssh-key", map[string]string{"Authorization": "Bearer " + s.cfg.Token})
	var wire controlclient.GeneratedKey
	if code != 200 || !strings.Contains(strings.ToLower(h.Get("Cache-Control")), "no-store") || json.Unmarshal(b, &wire) != nil || wire != k {
		s.t.Fatal("generated key response lacks no-store or differs from SDK retrieval")
	}
	p := filepath.Join(s.t.TempDir(), "identity")
	if os.WriteFile(p, []byte(k.PrivateKey), 0600) != nil {
		s.t.Fatal("cannot write temporary identity")
	}
	s.keys[id] = p
	return p
}

func makeIdentity(t *testing.T) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("identity generation failed")
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal("identity encoding failed")
	}
	p := filepath.Join(t.TempDir(), "identity")
	if os.WriteFile(p, pem.EncodeToMemory(block), 0600) != nil {
		t.Fatal("identity write failed")
	}
	pk, _ := ssh.NewPublicKey(pub)
	return p, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
}

func TestConfiguredSwaggerCreateExample(t *testing.T) {
	s := newSuite(t)
	s.preflight()
	code, _, body := s.get("/openapi.yaml", nil)
	if code != 200 {
		t.Fatal("published document unavailable")
	}
	var generic map[interface{}]interface{}
	if yaml.Unmarshal(body, &generic) != nil {
		t.Fatal("invalid document")
	}
	components, _ := generic["components"].(map[interface{}]interface{})
	schemas, _ := components["schemas"].(map[interface{}]interface{})
	create, _ := schemas["CreateVM"].(map[interface{}]interface{})
	example, ok := create["example"].(map[interface{}]interface{})
	if !ok {
		t.Fatal("published CreateVM has no configured example")
	}
	m := map[string]interface{}{}
	for k, v := range example {
		key, ok := k.(string)
		if !ok {
			t.Fatal("invalid example field")
		}
		m[key] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal("invalid example")
	}
	var r controlclient.CreateRequest
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || r.ImageID == "" || r.Profile == "" || r.SSHPublicKey != "" {
		t.Fatal("configured example is not executable generated-key profile contract")
	}
	// Execute exactly the published body; do not silently repair its image/profile/TTL.
	a := s.admitRequest(r)
	s.mustWait("published example", a.OperationID)
	s.guestFor(a.VMID)
}

func TestSuppliedKeyLogin(t *testing.T) {
	s := newSuite(t)
	s.preflight()
	key, pub := makeIdentity(t)
	r := s.request("supplied", s.cfg.BaseImageID)
	r.SSHPublicKey = pub
	a := s.admitRequest(r)
	s.keys[a.VMID] = key
	s.mustWait("supplied create", a.OperationID)
	s.guestFor(a.VMID)
	_, err := s.c.SSHKey(s.ctx, s.owner, a.VMID)
	var e *controlclient.APIError
	if !errors.As(err, &e) || e.Status != 409 || e.Code != "key_not_generated" {
		t.Fatalf("supplied key retrieval: %v, want 409 key_not_generated", err)
	}
}

func TestForeignKeyRetrieval(t *testing.T) {
	s := newSuite(t)
	tokenFile := strings.TrimSpace(os.Getenv("VMAPI_E2E_OTHER_TOKEN_FILE"))
	uidText := strings.TrimSpace(os.Getenv("VMAPI_E2E_OTHER_USER_ID"))
	if tokenFile == "" && uidText == "" {
		t.Skip("owner isolation requires explicit OTHER_TOKEN_FILE and OTHER_USER_ID fixtures")
	}
	uid, err := strconv.ParseInt(uidText, 10, 64)
	if err != nil || uid <= 0 || uid == s.owner {
		t.Fatal("invalid distinct OTHER_USER_ID")
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		t.Fatal("cannot read OTHER_TOKEN_FILE")
	}
	other, err := controlclient.NewUser(s.cfg.BaseURL, strings.TrimSpace(string(token)), uid, s.hc)
	if err != nil {
		t.Fatal("invalid other principal fixture")
	}
	if _, err := other.ListVMs(s.ctx, uid); err != nil {
		t.Fatalf("other principal authentication: %v", err)
	}
	s.preflight()
	a := s.createActive("foreign", s.cfg.BaseImageID)
	s.generatedKeyFile(a.VMID)
	k, err := other.SSHKey(s.ctx, uid, a.VMID)
	if (!isStatus(err, 404) && !isStatus(err, 403)) || k.PrivateKey != "" || k.PublicKey != "" {
		t.Fatal("foreign key retrieval leaked data or did not refuse")
	}
	// Raw response must not contain either key, not just fail SDK decoding.
	ownerKey, e := s.c.SSHKey(s.ctx, s.owner, a.VMID)
	if e != nil {
		t.Fatal(e)
	}
	code, _, body := s.get("/v1/vms/"+a.VMID+"/ssh-key", map[string]string{"Authorization": "Bearer " + strings.TrimSpace(string(token))})
	if (code != 403 && code != 404) || strings.Contains(string(body), ownerKey.PrivateKey) || strings.Contains(string(body), ownerKey.PublicKey) {
		t.Fatal("foreign raw response leaks key material")
	}
}

// ttlClockTolerance allows modest client/server clock skew and timestamp rounding.
// It must not mask a one-second lifetime for a thirty-second request.
const ttlClockTolerance = 2 * time.Second

func validateAdmissionDeadline(before, after time.Time, ttl time.Duration, deadline time.Time) error {
	earliest := before.Add(ttl - ttlClockTolerance)
	latest := after.Add(ttl + ttlClockTolerance)
	if deadline.Before(earliest) || deadline.After(latest) {
		return fmt.Errorf("finite admission deadline %s outside [%s, %s]", deadline, earliest, latest)
	}
	return nil
}

// waitGoneNotBefore rejects an observed early 404 immediately rather than waiting
// out the requested TTL and accepting the same disappearance later. The caller
// bounds polling with ctx; now is injectable for deterministic boundary tests.
func waitGoneNotBefore(ctx context.Context, interval time.Duration, earliest time.Time, get func(context.Context) error, now func() time.Time) error {
	return pollUntil(ctx, interval, func(ctx context.Context) (bool, error) {
		err := get(ctx)
		if isStatus(err, 404) {
			observed := now()
			if observed.Before(earliest) {
				return false, permanent(fmt.Errorf("VM disappeared at %s before earliest valid expiry %s", observed, earliest))
			}
			return true, nil
		}
		if err == nil {
			return false, errors.New("resource still visible")
		}
		return false, err
	})
}

func TestFiniteTTL(t *testing.T) {
	s := newSuite(t)
	s.preflight()
	ttl, e := policyTTL(s.caps, 30)
	if e != nil {
		t.Fatal(e)
	}
	r := s.request("ttl", s.cfg.BaseImageID)
	r.TTLSeconds = ttl
	before := time.Now()
	a, e := s.c.Create(s.ctx, s.owner, s.n.key("policy"), r)
	after := time.Now()
	// Track before any assertion, including a partial-success response.
	s.trackAccepted(acceptedCreate, a)
	if e != nil || a.VMID == "" {
		t.Fatalf("create TTL VM: %v (VM ID present: %t)", e, a.VMID != "")
	}
	requestedTTL := time.Duration(ttl) * time.Second
	earliest := before.Add(requestedTTL - ttlClockTolerance)
	vm, e := s.c.VM(s.ctx, s.owner, a.VMID)
	if e != nil || vm.ExpiresAt == nil || vm.TTLSeconds != ttl {
		t.Fatal("finite admission deadline missing or incorrect")
	}
	if e := validateAdmissionDeadline(before, after, requestedTTL, *vm.ExpiresAt); e != nil {
		t.Fatal(e)
	}
	deadline := *vm.ExpiresAt
	if os.Getenv("VMAPI_E2E_TTL_RESTART") == "1" {
		if s.cfg.RestartUnit == "" {
			t.Fatal("TTL_RESTART needs explicit RESTART_USER_UNIT")
		}
		restartAPI(s)
		after, e := s.c.VM(s.ctx, s.owner, a.VMID)
		if e == nil && (after.ExpiresAt == nil || !after.ExpiresAt.Equal(deadline)) {
			t.Fatal("restart reset TTL deadline")
		}
		if isStatus(e, 404) {
			if time.Now().Before(earliest) {
				t.Fatal("VM disappeared on restart readback before earliest valid expiry")
			}
			t.Skip("VM already expired on restart readback; deadline persistence across restart NOT proven")
		}
		if e != nil {
			t.Fatal(e)
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
	defer cancel()
	if err := waitGoneNotBefore(ctx, time.Second, earliest, func(ctx context.Context) error {
		_, e := s.c.VM(ctx, s.owner, a.VMID)
		return e
	}, time.Now); err != nil {
		t.Fatalf("expired VM should disappear no earlier than requested TTL (within clock tolerance): %v", err)
	}
}

func restartAPI(s *suite) {
	ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
	defer cancel()
	if _, err := run(ctx, "", "systemctl", "--user", "restart", s.cfg.RestartUnit); err != nil {
		s.t.Fatal("explicit API restart failed")
	}
	if err := pollUntil(ctx, time.Second, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", s.cfg.BaseURL+"/healthz", nil)
		if err != nil {
			return false, err
		}
		resp, err := s.hc.Do(req)
		if err != nil {
			return false, err
		}
		resp.Body.Close()
		return resp.StatusCode == 200, nil
	}); err != nil {
		s.t.Fatal("API did not recover after restart")
	}
}

func TestProfileAndQuotaMetadata(t *testing.T) {
	s := newSuite(t)
	s.preflight()
	if s.caps.MaxVMs < 1 || s.caps.MaxVMsPerUser < 1 || !s.caps.SSHGeneratedKeySupported || !s.caps.SSHPublicKeySupported {
		t.Fatal("required quota/SSH capabilities absent")
	}
	var profile *controlclient.Profile
	for i := range s.caps.Profiles {
		if s.caps.Profiles[i].Name == s.caps.DefaultProfile {
			profile = &s.caps.Profiles[i]
		}
	}
	if profile == nil || profile.CPULimit <= 0 || profile.CPULimit > float64(profile.VCPU) {
		t.Fatal("default profile has invalid CPU limit")
	}
	a := s.createActive("profile", s.cfg.BaseImageID)
	vm, e := s.c.VM(s.ctx, s.owner, a.VMID)
	if e != nil || vm.Profile != profile.Name || vm.Resources.CPULimit != profile.CPULimit || vm.Resources.VCPU != profile.VCPU || vm.Resources.MemoryMiB != profile.MemoryMiB {
		t.Fatal("effective VM profile/reservation differs from capabilities")
	}
	t.Log("public reservation metadata verified; this is NOT physical CPU cgroup evidence")
}

func TestTTLBusyCreate(t *testing.T) {
	s := newSuite(t)
	s.preflight()
	r := s.request("busyttl", s.cfg.BaseImageID)
	r.TTLSeconds = 1
	a := s.admitRequest(r)
	witnessed := false
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	err := pollUntil(ctx, 100*time.Millisecond, func(ctx context.Context) (bool, error) {
		vm, e := s.c.VM(ctx, s.owner, a.VMID)
		if isStatus(e, 404) {
			op, e := s.c.Operation(ctx, s.owner, a.OperationID)
			if e != nil {
				return false, e
			}
			if op.Status == "pending" || op.Status == "running" {
				return false, permanent(errors.New("VM disappeared before Create operation settled"))
			}
			return true, nil
		}
		if e != nil {
			return false, e
		}
		op, e := s.c.Operation(ctx, s.owner, a.OperationID)
		if e != nil {
			return false, e
		}
		if !witnessed && vm.ExpiresAt != nil && time.Now().After(*vm.ExpiresAt) && (op.Status == "pending" || op.Status == "running") {
			witnessed = true
			// Expiry must refuse ordinary work even while the Create child settles.
			acc, e := s.c.Stop(ctx, s.owner, a.VMID, s.n.key("expired-stop"))
			s.trackAccepted(acceptedReferences, acc)
			var apiErr *controlclient.APIError
			if !errors.As(e, &apiErr) || apiErr.Code != "vm_expired" {
				return false, permanent(fmt.Errorf("expired busy VM command: %v", e))
			}
		}
		return op.Status == "succeeded" || op.Status == "failed", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.requireGone(func(ctx context.Context) error { _, e := s.c.VM(ctx, s.owner, a.VMID); return e }, "busy expired VM")
	if !witnessed {
		t.Skip("Create settled before observable expiry; busy-child deferral NOT proven (requires controlled slow real backend)")
	}
	t.Log("observed expired VM retained during pending/running Create, command refused, then safe disappearance; no process-level no-preemption claim")
}
