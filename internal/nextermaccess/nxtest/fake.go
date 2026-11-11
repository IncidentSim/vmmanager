// Package nxtest is an in-memory stand-in for the stock Nexterm HTTP contracts
// used by nextermaccess, for tests only. It reproduces the quirks that matter:
// domain errors with HTTP 200, no ID from PUT /api/users, and POST /api/auth/logout
// destroying EVERY session of the account (it is a trap; callers must not use it).
package nxtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

type User struct {
	Name, Pass string
	Admin      bool
}
type Identity struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Username string `json:"username"`
}

type Fake struct {
	Mu                sync.Mutex
	Users             map[int64]*User
	Sessions          map[string]int64 // token -> account
	SessionID         map[string]int64 // token -> session id
	NextID            int64
	FailCreate        bool // PUT /api/users -> 200 {"code":500}
	MonitoringUpdates int
	FailMonitoring    bool
	IgnoreMonitoring  bool
	BadEntryIP        bool
	AdminImpers       bool // impersonated ordinary accounts claim isAdmin
	OmitIsAdmin       bool // /me omits the isAdmin field
	FailImpers        bool // POST /api/users/{id}/login -> domain error
	GlobalLogout      bool // set when the destructive POST /api/auth/logout is called
	Identities        map[int64]map[int64]Identity
	Entries           map[int64]map[int64]map[string]any
}

func New() *Fake {
	return &Fake{Users: map[int64]*User{1: {"root", "adminpw", true}}, Sessions: map[string]int64{}, SessionID: map[string]int64{}, NextID: 10,
		Identities: map[int64]map[int64]Identity{}, Entries: map[int64]map[int64]map[string]any{}}
}

// NonAdmin counts ordinary accounts.
func (f *Fake) NonAdmin() (n int) {
	for _, u := range f.Users {
		if !u.Admin {
			n++
		}
	}
	return
}

// Login opens a session for an account (a "real terminal" session in tests).
func (f *Fake) Login(id int64) string {
	f.NextID++
	t := "tok" + strconv.FormatInt(f.NextID, 10) + "z" + strconv.FormatInt(id, 10)
	f.Sessions[t] = id
	f.SessionID[t] = f.NextID
	return t
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	out := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	dom := func(c int) { out(map[string]any{"code": c, "message": "x"}) }
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	uid, authed := f.Sessions[tok]
	p := r.URL.Path
	seg := func(i int) int64 { n, _ := strconv.ParseInt(strings.Split(p, "/")[i], 10, 64); return n }
	switch {
	case p == "/api/auth/login":
		for id, u := range f.Users {
			if u.Name == body["username"] && u.Pass == body["password"] {
				out(map[string]any{"token": f.Login(id)})
				return
			}
		}
		dom(201)
	case p == "/api/auth/logout": // stock: kills all sessions of the account
		f.GlobalLogout = true
		if id, ok := f.Sessions[fmt.Sprint(body["token"])]; ok {
			for t, o := range f.Sessions {
				if o == id {
					delete(f.Sessions, t)
				}
			}
		}
		out(map[string]any{"message": "ok"})
	case !authed:
		w.WriteHeader(401)
		out(map[string]any{"message": "bad token"})
	case p == "/api/sessions/list":
		l := []map[string]any{}
		for t, o := range f.Sessions {
			if o == uid {
				l = append(l, map[string]any{"id": f.SessionID[t], "current": t == tok})
			}
		}
		out(l)
	case strings.HasPrefix(p, "/api/sessions/") && r.Method == "DELETE":
		id := seg(3)
		for t, o := range f.Sessions {
			if o == uid && f.SessionID[t] == id {
				delete(f.Sessions, t)
				out(map[string]any{"message": "ok"})
				return
			}
		}
		dom(206)
	case p == "/api/accounts/me":
		u := f.Users[uid]
		m := map[string]any{"id": uid, "username": u.Name}
		if !f.OmitIsAdmin {
			m["isAdmin"] = u.Admin || (f.AdminImpers && !u.Admin)
		}
		out(m)
	case p == "/api/users/list":
		var us []map[string]any
		for id, u := range f.Users {
			if strings.Contains(u.Name, r.URL.Query().Get("search")) {
				us = append(us, map[string]any{"id": id, "username": u.Name})
			}
		}
		out(map[string]any{"users": us, "total": len(us)})
	case p == "/api/users" && r.Method == "PUT":
		if f.FailCreate {
			dom(500)
			return
		}
		for _, u := range f.Users {
			if u.Name == body["username"] {
				dom(101)
				return
			}
		}
		f.NextID++
		f.Users[f.NextID] = &User{Name: fmt.Sprint(body["username"]), Pass: fmt.Sprint(body["password"])}
		out(map[string]any{"message": "Account got successfully created"})
	case strings.HasPrefix(p, "/api/users/") && strings.HasSuffix(p, "/login"):
		id := seg(3)
		if f.Users[id] == nil || f.FailImpers {
			dom(102)
			return
		}
		out(map[string]any{"token": f.Login(id)})
	case strings.HasPrefix(p, "/api/users/") && r.Method == "DELETE":
		id := seg(3)
		if f.Users[id] == nil {
			dom(102)
			return
		}
		delete(f.Users, id)
		delete(f.Identities, id)
		delete(f.Entries, id)
		for t, o := range f.Sessions {
			if o == id {
				delete(f.Sessions, t)
			}
		}
		out(map[string]any{"message": "deleted"})
	case p == "/api/identities/list":
		l := []Identity{}
		for _, i := range f.Identities[uid] {
			l = append(l, i)
		}
		out(l)
	case p == "/api/identities" && r.Method == "PUT":
		if body["type"] != "ssh" || !strings.HasPrefix(fmt.Sprint(body["sshKey"]), "-----BEGIN") {
			dom(400)
			return
		}
		f.NextID++
		if f.Identities[uid] == nil {
			f.Identities[uid] = map[int64]Identity{}
		}
		f.Identities[uid][f.NextID] = Identity{ID: f.NextID, Name: fmt.Sprint(body["name"]), Type: "ssh", Username: fmt.Sprint(body["username"])}
		out(map[string]any{"message": "ok", "id": f.NextID})
	case strings.HasPrefix(p, "/api/identities/") && r.Method == "DELETE":
		delete(f.Identities[uid], seg(3))
		out(map[string]any{"message": "ok"})
	case p == "/api/entries/list":
		l := []map[string]any{}
		for _, e := range f.Entries[uid] {
			l = append(l, e)
		}
		out([]any{map[string]any{"id": "folder-1", "type": "folder", "entries": l}})
	case p == "/api/entries" && r.Method == "PUT":
		f.NextID++
		cfg, _ := body["config"].(map[string]any)
		if f.BadEntryIP {
			cfg["ip"] = "10.9.9.9"
		}
		if f.Entries[uid] == nil {
			f.Entries[uid] = map[int64]map[string]any{}
		}
		f.Entries[uid][f.NextID] = map[string]any{"id": f.NextID, "name": body["name"], "config": cfg, "identities": body["identities"]}
		out(map[string]any{"message": "ok", "id": f.NextID})
	case strings.HasPrefix(p, "/api/entries/") && r.Method == "PATCH":
		e := f.Entries[uid][seg(3)]
		cfg, ok := body["config"].(map[string]any)
		if e == nil || !ok || cfg["protocol"] != "ssh" || cfg["monitoringEnabled"] != true || len(body) != 1 || f.Users[uid].Admin {
			dom(400)
			return
		}
		f.MonitoringUpdates++
		if f.FailMonitoring {
			dom(500)
			return
		}
		if !f.IgnoreMonitoring {
			e["config"] = cfg // stock replaces, rather than merges, the JSON config
		}
		out(map[string]any{"message": "ok"})
	case strings.HasPrefix(p, "/api/entries/") && r.Method == "GET":
		e := f.Entries[uid][seg(3)]
		if e == nil {
			dom(401)
			return
		}
		out(e)
	case strings.HasPrefix(p, "/api/entries/") && r.Method == "DELETE":
		delete(f.Entries[uid], seg(3))
		out(map[string]any{"message": "ok"})
	default:
		w.WriteHeader(404)
	}
}
