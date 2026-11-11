package controlapi

import (
	"context"
	"errors"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"net/http"
)

// GeneratedKey is returned only by the authenticated owner-only endpoint.
// Implementations must load from the encrypted credential store, not workflow history.
type GeneratedKey struct {
	PrivateKeyPEM []byte
	PublicKey     string
}
type KeyProvider interface {
	GeneratedKey(context.Context, int64, string) (GeneratedKey, error)
}

var ErrKeyNotGenerated = errors.New("key not generated")

type Options struct {
	Policy Policy
	Keys   KeyProvider
	// Browser, when set, supplies the unified Nexterm browser link for the
	// terminal operation result and GET /v1/vms/{id}/ssh (replaces ttyd URLs).
	Browser BrowserLinker
	// ErrorMapper maps lower-layer sentinel errors without exposing their messages.
	ErrorMapper func(error) (status int, code string, ok bool)
}

// Configure must be called before serving requests. Flags assert real lower-layer support.
func (s *Server) Configure(o Options) error {
	if o.Policy.DefaultProfile == "" && s.ctl != nil {
		o.Policy.DefaultProfile = s.ctl.DefaultProfile()
	}
	if o.Policy.DefaultProfile == "" {
		o.Policy.DefaultProfile = "small"
	}
	if err := o.Policy.Validate(); err != nil {
		return err
	}
	if o.Policy.GeneratedSSHKey && o.Keys == nil {
		return errors.New("generated SSH support requires key provider")
	}
	s.opts = o
	return nil
}
func (s *Server) fail(w http.ResponseWriter, e error) {
	if s.opts.ErrorMapper != nil {
		if status, code, ok := s.opts.ErrorMapper(e); ok && status >= 400 && status <= 599 {
			problem(w, status, code)
			return
		}
	}
	if errors.Is(e, vmbot.ErrExpired) {
		problem(w, 409, "vm_expired")
		return
	}
	if errors.Is(e, vmlib.ErrUnknownProfile) {
		respond(w, 422, map[string]any{"error": map[string]any{"code": "profile_unknown", "message": "unknown profile", "fields": []FieldError{fe("profile", "profile_unknown", "select a profile from capabilities")}}})
		return
	}
	if errors.Is(e, ErrKeyNotGenerated) {
		problem(w, 409, "key_not_generated")
		return
	}
	fail(w, e)
}
func validationProblem(w http.ResponseWriter, fields []FieldError) {
	respond(w, 422, map[string]any{"error": map[string]any{"code": "validation_failed", "message": "request validation failed", "fields": fields}})
}
func routeAllow(parts []string) string {
	if len(parts) == 2 {
		switch parts[1] {
		case "vms":
			return "GET, POST"
		case "images", "capabilities":
			return "GET"
		}
	}
	if len(parts) == 3 {
		switch parts[1] {
		case "vms", "images":
			return "DELETE, GET"
		case "operations":
			return "GET"
		}
	}
	if len(parts) == 4 && parts[1] == "vms" {
		switch parts[3] {
		case "ssh", "ssh-key":
			return "GET"
		case "stop", "commit", "terminal":
			return "POST"
		}
	}
	return ""
}
