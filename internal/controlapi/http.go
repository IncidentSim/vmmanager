// Package controlapi exposes the authenticated v1 control protocol. It has no
// scheduler: durable admission and all effects belong to the DBOS controller.
package controlapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/identity"
	"github.com/petruekhin/vibe-firecracker/internal/jwtauth"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type Credential struct {
	SHA256    string `json:"sha256,omitempty"`
	TokenFile string `json:"token_file,omitempty"`
	UserID    int64  `json:"user_id,omitempty"`
	Service   bool   `json:"service,omitempty"`
}
type Server struct {
	ctl         *vmbot.Controller
	credentials []Credential
	jwt         *JWTAuth
	degraded    atomic.Bool
}

// JWTAuth verifies bot-signed user tokens with the PUBLIC key only. A token's
// subject is the immutable numeric owner. The optional username claim (covered
// by the signature) may bootstrap the matching configured username slot, which
// is then pinned to that numeric ID in the durable binding store.
type JWTAuth struct {
	Verifier  *jwtauth.Verifier
	Bindings  *identity.Bindings
	Usernames []string // canonical configured allowlist
	Now       func() time.Time

	mu sync.RWMutex
}

func (j *JWTAuth) usernames() []string {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.Usernames
}

// SetUsernames replaces the configured allowlist (canonical names).
func (j *JWTAuth) SetUsernames(n []string) {
	j.mu.Lock()
	j.Usernames = append([]string(nil), n...)
	j.mu.Unlock()
}
func (j *JWTAuth) configured(name string) bool {
	for _, n := range j.usernames() {
		if n == name {
			return true
		}
	}
	return false
}

// UserAllowed reports whether id owns a pinned username slot that is still in
// the configured allowlist. Revoking a username (config + restart) removes
// access here, which also fences restart projection, without deleting any VM.
func (j *JWTAuth) UserAllowed(id int64) bool {
	return j.Bindings != nil && j.Bindings.BoundTo(id, j.usernames())
}

// SetDegraded marks that startup repair could not re-drive every durable
// operation. The API keeps serving (operation status stays queryable); only the
// unauthenticated health text changes. It never reveals which operation.
func (s *Server) SetDegraded(v bool) { s.degraded.Store(v) }
func (s *Server) Degraded() bool     { return s.degraded.Load() }

func New(c *vmbot.Controller, credentials []Credential) (*Server, error) {
	return NewWithJWT(c, credentials, nil)
}

// NewWithJWT is New plus optional JWT user authentication. With a verifier the
// static credential list may be empty.
func NewWithJWT(c *vmbot.Controller, credentials []Credential, j *JWTAuth) (*Server, error) {
	if j != nil {
		if j.Verifier == nil || j.Bindings == nil {
			return nil, errors.New("jwt verifier and username bindings required")
		}
		c.SetControlAuthorizer(j.UserAllowed)
	}
	if len(credentials) == 0 && j == nil {
		return nil, errors.New("credentials required")
	}
	seen := map[string]bool{}
	for _, x := range credentials {
		b, e := hex.DecodeString(x.SHA256)
		if e != nil || len(b) != 32 || seen[x.SHA256] || (!x.Service && !c.ControlAllowed(x.UserID)) || (x.Service && x.UserID != 0) {
			return nil, errors.New("invalid credential configuration")
		}
		seen[x.SHA256] = true
	}
	p := c.ControlPolicy()
	if p.MaxVMs <= 0 || p.MaxVMsPerUser <= 0 || (len(p.AllowedUsers) == 0 && !c.HasControlAuthorizer()) {
		return nil, errors.New("positive quotas and allowlist required")
	}
	seenUsers := map[int64]bool{}
	for _, u := range p.AllowedUsers {
		if u <= 0 || seenUsers[u] {
			return nil, errors.New("invalid allowlist")
		}
		seenUsers[u] = true
	}
	for _, id := range p.SharedImages {
		if vmlib.ValidateID("image", id) != nil {
			return nil, errors.New("invalid shared image")
		}
	}
	return &Server{ctl: c, credentials: append([]Credential{}, credentials...), jwt: j}, nil
}
func respond(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, code int, name string) {
	respond(w, code, map[string]any{"error": map[string]string{"code": name, "message": strings.ReplaceAll(name, "_", " ")}})
}
func fail(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, vmbot.ErrNoVM):
		problem(w, 404, "not_found")
	case errors.Is(e, vmbot.ErrControlForbidden):
		problem(w, 403, "forbidden")
	case errors.Is(e, vmbot.ErrCapacity), errors.Is(e, vmlib.ErrResourceExhausted):
		problem(w, 429, "capacity")
	case errors.Is(e, vmlib.ErrInvalidArgument):
		problem(w, 422, "invalid_argument")
	case errors.Is(e, vmbot.ErrControlConflict), errors.Is(e, vmbot.ErrNotRunning):
		problem(w, 409, "conflict")
	default:
		problem(w, 503, "unavailable")
	}
}
func (s *Server) authenticate(r *http.Request) (int64, int) {
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 || !strings.HasPrefix(headers[0], "Bearer ") {
		return 0, 401
	}
	token := strings.TrimPrefix(headers[0], "Bearer ")
	if token == "" || len(token) > 4096 {
		return 0, 401
	}
	sum := sha256.Sum256([]byte(token))
	var found *Credential
	for i := range s.credentials {
		b, _ := hex.DecodeString(s.credentials[i].SHA256)
		if subtle.ConstantTimeCompare(sum[:], b) == 1 {
			found = &s.credentials[i]
		}
	}
	if found == nil {
		if s.jwt != nil {
			return s.authenticateJWT(r, token)
		}
		return 0, 401
	}
	acting := r.Header.Values("X-Acting-User-ID")
	u := found.UserID
	if found.Service {
		if len(acting) != 1 {
			return 0, 403
		}
		var e error
		u, e = strconv.ParseInt(acting[0], 10, 64)
		if e != nil || u <= 0 || strconv.FormatInt(u, 10) != acting[0] {
			return 0, 403
		}
	} else if len(acting) != 0 {
		return 0, 403
	}
	if !s.ctl.ControlAllowed(u) {
		return 0, 403
	}
	return u, 0
}
func (s *Server) authenticateJWT(r *http.Request, token string) (int64, int) {
	now := time.Now()
	if s.jwt.Now != nil {
		now = s.jwt.Now()
	}
	claims, err := s.jwt.Verifier.Verify(token, now)
	if err != nil {
		return 0, 401
	}
	// A user token never selects another acting user.
	if len(r.Header.Values("X-Acting-User-ID")) != 0 {
		return 0, 403
	}
	id := claims.Subject
	if claims.Username != "" && s.jwt.configured(claims.Username) {
		// Bootstrap/verify the pinned slot from the signed claim.
		if s.jwt.Bindings.Claim(claims.Username, id) != nil {
			return 0, 403
		}
	} else if claims.Username != "" {
		// An unlisted handle can never grant access, and must not contradict
		// a pin held by someone else.
		if owner, ok := s.jwt.Bindings.Owner(claims.Username); ok && owner != id {
			return 0, 403
		}
	}
	if !s.ctl.ControlAllowed(id) {
		return 0, 403
	}
	return id, 0
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		problem(w, 422, "invalid_argument")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		problem(w, 422, "invalid_argument")
		return false
	}
	return true
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if serveDocs(w, r) {
		return
	}
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		status := "ok"
		if s.degraded.Load() {
			status = "degraded"
		}
		respond(w, 200, map[string]string{"status": status})
		return
	}
	u, status := s.authenticate(r)
	if status != 0 {
		if status == 401 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			problem(w, status, "unauthorized")
		} else {
			problem(w, status, "forbidden")
		}
		return
	}
	if r.URL.RawQuery != "" {
		problem(w, 422, "invalid_argument")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		problem(w, 404, "not_found")
		return
	}
	if r.Method == "GET" {
		switch {
		case len(parts) == 2 && parts[1] == "vms":
			respond(w, 200, map[string]any{"vms": s.ctl.ControlVMs(u)})
			return
		case len(parts) == 2 && parts[1] == "images":
			respond(w, 200, map[string]any{"images": s.publicImages(u)})
			return
		case len(parts) == 2 && parts[1] == "capabilities":
			bc, e := s.ctl.ControlCapabilities(r.Context(), u)
			if e != nil {
				fail(w, e)
				return
			}
			// max_vms/max_vms_per_user are the API's owner/global quotas; the
			// backend's own limits are reported separately, never merged.
			p := s.ctl.ControlPolicy()
			profiles := make([]map[string]any, 0, len(bc.Profiles))
			for _, x := range bc.Profiles {
				profiles = append(profiles, map[string]any{"name": x.Name, "vcpu": x.VCPU, "memory_mib": x.MemMiB, "disk_mib": x.DiskMiB})
			}
			out := map[string]any{"max_vms": p.MaxVMs, "max_vms_per_user": p.MaxVMsPerUser, "profiles": profiles,
				"backend_max_vms": bc.MaxVMs, "ssh_public_key_supported": false}
			if bc.MaxTotalMemMiB > 0 {
				out["max_total_memory_mib"] = bc.MaxTotalMemMiB
			}
			if bc.MaxTotalVCPU > 0 {
				out["max_total_vcpu"] = bc.MaxTotalVCPU
			}
			respond(w, 200, out)
			return
		case len(parts) == 3 && parts[1] == "vms":
			v, e := s.ctl.ControlVM(u, parts[2])
			if e != nil {
				fail(w, e)
			} else {
				respond(w, 200, v)
			}
			return
		case len(parts) == 4 && parts[1] == "vms" && parts[3] == "ssh":
			x, e := s.ctl.ControlSSH(r.Context(), u, parts[2])
			if e != nil {
				fail(w, e)
				return
			}
			out := map[string]any{"host": x.Host, "port": x.Port, "user": x.User, "host_key_fingerprint": x.HostKeyFingerprint, "ready": x.Ready, "auth_mode": x.AuthMode, "key_reference": x.KeyReference}
			if x.Bastion != nil {
				out["bastion"] = map[string]any{"host": x.Bastion.Host, "port": x.Bastion.Port, "user": x.Bastion.User}
			}
			respond(w, 200, out)
			return
		case len(parts) == 3 && parts[1] == "images":
			for _, i := range s.publicImages(u) {
				if i["id"] == parts[2] {
					respond(w, 200, i)
					return
				}
			}
			problem(w, 404, "not_found")
			return
		case len(parts) == 3 && parts[1] == "operations":
			o, e := s.ctl.ControlOperation(u, parts[2])
			if e != nil {
				fail(w, e)
				return
			}
			out := map[string]any{"id": o.ID, "kind": o.Kind, "status": o.Status}
			if o.VMID != "" {
				out["vm_id"] = o.VMID
			}
			if o.ImageID != "" {
				out["image_id"] = o.ImageID
			}
			if o.Status == "failed" {
				out["error"] = map[string]string{"code": o.ErrorCode, "message": "operation not confirmed"}
			}
			if o.Kind == "terminal" && o.Status == "succeeded" {
				url, e := s.ctl.ControlTerminalURL(u, o.VMID)
				if e == nil {
					out["result"] = map[string]string{"url": url}
				}
			}
			respond(w, 200, out)
			return
		}
		problem(w, 404, "not_found")
		return
	}
	req := vmbot.ControlRequest{}
	switch {
	case r.Method == "POST" && len(parts) == 2 && parts[1] == "vms":
		var b struct {
			Name         string                  `json:"name"`
			ImageID      string                  `json:"image_id"`
			Resources    *vmbot.ControlResources `json:"resources"`
			SSHPublicKey string                  `json:"ssh_public_key"`
		}
		if !decode(w, r, &b) {
			return
		}
		req = vmbot.ControlRequest{Kind: "create", Name: b.Name, ImageID: b.ImageID, Resources: b.Resources, SSHPublicKey: b.SSHPublicKey}
	case r.Method == "POST" && len(parts) == 4 && parts[1] == "vms" && (parts[3] == "stop" || parts[3] == "commit" || parts[3] == "terminal"):
		// Parsing is side-effect-free. Domain admission first resolves an
		// owner-scoped idempotency association, even after VM deletion, and
		// checks ownership before any genuinely new intent can be persisted.
		if parts[3] == "commit" {
			var b struct {
				Name string `json:"name"`
			}
			if !decode(w, r, &b) {
				return
			}
			req.Name = b.Name
		} else {
			var b struct{}
			if !decode(w, r, &b) {
				return
			}
		}
		req.Kind = parts[3]
		req.VMID = parts[2]
	case r.Method == "DELETE" && len(parts) == 3 && parts[1] == "vms":
		req.Kind = "delete"
		req.VMID = parts[2]
	case r.Method == "DELETE" && len(parts) == 3 && parts[1] == "images":
		req.Kind = "delete_image"
		req.ImageID = parts[2]
	default:
		problem(w, 404, "not_found")
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 {
		problem(w, 422, "invalid_argument")
		return
	}
	o, e := s.ctl.AcceptControl(u, keys[0], req)
	if e != nil {
		fail(w, e)
		return
	}
	out := map[string]any{"operation_id": o.ID}
	if o.VMID != "" {
		out["vm_id"] = o.VMID
	}
	if o.ImageID != "" {
		out["image_id"] = o.ImageID
	}
	respond(w, 202, out)
}
func (s *Server) publicImages(u int64) []map[string]any {
	out := []map[string]any{}
	for _, i := range s.ctl.ControlImages(u) {
		out = append(out, map[string]any{"id": i.ID, "name": i.Name, "shared": i.Shared})
	}
	return out
}
