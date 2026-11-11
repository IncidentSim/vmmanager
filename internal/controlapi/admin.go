package controlapi

import (
	"net/http"
	"strings"
)

func adminPath(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/v1/admin/") }
func (s *Server) adminRoute(w http.ResponseWriter, r *http.Request, u int64) {
	if !s.ctl.AdminAllowed(u) {
		problem(w, 403, "forbidden")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/admin/delete-all")
	if r.URL.Path == "/v1/admin/delete-all" && r.Method == "GET" {
		b, e := s.ctl.AdminBatches(u)
		if e != nil {
			s.fail(w, e)
			return
		}
		respond(w, 200, map[string]any{"batches": b})
		return
	}
	if r.URL.Path != "/v1/admin/delete-all" && (path == r.URL.Path || !strings.HasPrefix(path, "/") || strings.Contains(path[1:], "/")) {
		problem(w, 404, "not_found")
		return
	}
	if r.Method != "POST" {
		problem(w, 405, "method_not_allowed")
		return
	}
	var in struct {
		ChatID int64 `json:"chat_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.ChatID != u {
		problem(w, 403, "forbidden")
		return
	}
	if path == "" {
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || len(keys[0]) < 1 || len(keys[0]) > 128 {
			problem(w, 422, "invalid_argument")
			return
		}
		b, e := s.ctl.AdminPreview(u, in.ChatID, keys[0])
		if e != nil {
			s.fail(w, e)
			return
		}
		respond(w, 202, b)
	} else {
		token := strings.TrimPrefix(path, "/")
		if len(token) != 43 {
			problem(w, 404, "not_found")
			return
		}
		b, e := s.ctl.AdminAdvance(u, in.ChatID, token)
		if e != nil {
			s.fail(w, e)
			return
		}
		respond(w, 202, b)
	}
}
