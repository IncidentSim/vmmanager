// Package nextermaccess provisions one random, ordinary local STOCK Nexterm
// account per VM (with the VM's own generated SSH key as a personal identity and
// one SSH entry) and keeps the durable owner+VM binding needed for retry and
// cleanup. It never patches Nexterm, never uses LDAP/OIDC and never writes to the
// VM API database; VM facts come only through the owner-authorized control API.
//
// Nexterm quirks handled here: domain failures are JSON {"code":N,...} that are
// often returned with HTTP 200, PUT /api/users returns no account ID, and the
// stock server returns no error for an unknown path segment mismatch.
package nextermaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Secret never prints, formats or marshals its value.
type Secret string

func (Secret) String() string               { return "[redacted]" }
func (Secret) GoString() string             { return "[redacted]" }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, "[redacted]") }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Reveal returns the value; call it only when handing it to the owner.
func (s Secret) Reveal() string { return string(s) }

// DomainError is a Nexterm {"code":N} body. The message is deliberately dropped.
type DomainError struct{ Code int }

func (e *DomainError) Error() string { return "nexterm: domain error code " + strconv.Itoa(e.Code) }

// HTTPError is a non-2xx answer; the body is never retained.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return "nexterm: HTTP " + strconv.Itoa(e.Status) }

const (
	codeAlreadyExists = 101
	codeNoSuchAccount = 102
)

type Client struct {
	base string
	hc   *http.Client
}

// NewClient validates the base URL (https, or http only on a loopback host).
func NewClient(base string, hc *http.Client) (*Client, error) {
	if e := ValidateURL(base); e != nil {
		return nil, e
	}
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	cp := *hc
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(base, "/"), hc: &cp}, nil
}

// ValidateURL accepts only a bare origin.
func ValidateURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Hostname() == "" {
		return errors.New("invalid nexterm url")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
			return errors.New("nexterm url must be https (http only on loopback)")
		}
	default:
		return errors.New("invalid nexterm url")
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path, token string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return errors.New("nexterm: encode failed")
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if e != nil {
		return errors.New("nexterm: invalid request")
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := c.hc.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("nexterm: transport failed")
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if e != nil || len(b) > 2<<20 {
		return errors.New("nexterm: invalid response size")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Stock Nexterm also reports some domain errors with 4xx.
		var d struct {
			Code *int `json:"code"`
		}
		if json.Unmarshal(b, &d) == nil && d.Code != nil && *d.Code != 0 {
			return &DomainError{Code: *d.Code}
		}
		return &HTTPError{Status: resp.StatusCode}
	}
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		var d struct {
			Code *json.Number `json:"code"`
		}
		if json.Unmarshal(b, &d) == nil && d.Code != nil {
			n, _ := d.Code.Int64()
			if n == 0 {
				n = -1
			}
			return &DomainError{Code: int(n)}
		}
	}
	if out == nil {
		return nil
	}
	if len(b) == 0 || json.Unmarshal(b, out) != nil {
		return errors.New("nexterm: invalid response")
	}
	return nil
}

func goodToken(t string) bool {
	return t != "" && len(t) <= 4096 && !strings.ContainsAny(t, " \r\n\t")
}

// Login performs a normal password login (admin session, or proof that the
// delivered credentials work).
func (c *Client) Login(ctx context.Context, user string, pass Secret) (string, error) {
	var r struct {
		Token        string          `json:"token"`
		TOTPRequired json.RawMessage `json:"totpRequired"`
	}
	if e := c.do(ctx, "POST", "/api/auth/login", "", map[string]string{"username": user, "password": pass.Reveal()}, &r); e != nil {
		return "", e
	}
	// Stock Sequelize responses use 0/1; fixtures and other releases use bool.
	// Only an absent flag, false or exactly zero permit a password-only login.
	flag := string(bytes.TrimSpace(r.TOTPRequired))
	if !goodToken(r.Token) || (flag != "" && flag != "false" && flag != "0") {
		return "", errors.New("nexterm: login unusable")
	}
	return r.Token, nil
}

// CloseSession destroys exactly the session behind token. Stock POST
// /api/auth/logout would also destroy every other session of the account (live
// terminals), so it is never used: list sessions, delete the one flagged
// "current" by its ID, then require the token to be rejected.
func (c *Client) CloseSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	var list []struct {
		ID      int64 `json:"id"`
		Current bool  `json:"current"`
	}
	if e := c.do(ctx, "GET", "/api/sessions/list", token, nil, &list); e != nil {
		return e
	}
	var id int64
	for _, s := range list {
		if s.Current {
			if id != 0 || s.ID <= 0 {
				return errors.New("nexterm: ambiguous current session")
			}
			id = s.ID
		}
	}
	if id == 0 {
		return errors.New("nexterm: current session not found")
	}
	if e := c.do(ctx, "DELETE", "/api/sessions/"+strconv.FormatInt(id, 10), token, nil, nil); e != nil {
		return e
	}
	var h *HTTPError
	if e := c.do(ctx, "GET", "/api/accounts/me", token, nil, nil); !errors.As(e, &h) || h.Status != 401 {
		return errors.New("nexterm: session still valid after close")
	}
	return nil
}

type Me struct {
	ID       int64
	Username string
	IsAdmin  bool
}

func (c *Client) Me(ctx context.Context, token string) (Me, error) {
	var w struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		IsAdmin  *bool  `json:"isAdmin"`
	}
	if e := c.do(ctx, "GET", "/api/accounts/me", token, nil, &w); e != nil {
		return Me{}, e
	}
	// A missing isAdmin is treated as unproven, never as "ordinary".
	if w.ID <= 0 || w.Username == "" || w.IsAdmin == nil {
		return Me{}, errors.New("nexterm: invalid account readback")
	}
	return Me{ID: w.ID, Username: w.Username, IsAdmin: *w.IsAdmin}, nil
}

// FindUser returns the exact-username account ID (0 when absent).
func (c *Client) FindUser(ctx context.Context, admin, username string) (int64, error) {
	var r struct {
		Users []struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"users"`
	}
	q := url.Values{"search": {username}, "limit": {"100"}}
	if e := c.do(ctx, "GET", "/api/users/list?"+q.Encode(), admin, nil, &r); e != nil {
		return 0, e
	}
	var id int64
	for _, u := range r.Users {
		if u.Username == username {
			if id != 0 || u.ID <= 0 {
				return 0, errors.New("nexterm: ambiguous account readback")
			}
			id = u.ID
		}
	}
	return id, nil
}

// ErrExists reports a username collision from PUT /api/users.
var ErrExists = errors.New("nexterm: account exists")

func (c *Client) CreateUser(ctx context.Context, admin, username string, pass Secret) error {
	e := c.do(ctx, "PUT", "/api/users", admin, map[string]string{"username": username, "password": pass.Reveal(), "firstName": "VM", "lastName": "Owner"}, nil)
	var d *DomainError
	if errors.As(e, &d) && d.Code == codeAlreadyExists {
		return ErrExists
	}
	return e
}

func (c *Client) Impersonate(ctx context.Context, admin string, id int64) (string, error) {
	var r struct {
		Token string `json:"token"`
	}
	if e := c.do(ctx, "POST", "/api/users/"+strconv.FormatInt(id, 10)+"/login", admin, struct{}{}, &r); e != nil {
		return "", e
	}
	if !goodToken(r.Token) {
		return "", errors.New("nexterm: impersonation unusable")
	}
	return r.Token, nil
}

// DeleteUser treats an already-missing account as success.
func (c *Client) DeleteUser(ctx context.Context, admin string, id int64) error {
	e := c.do(ctx, "DELETE", "/api/users/"+strconv.FormatInt(id, 10), admin, nil, nil)
	var d *DomainError
	if errors.As(e, &d) && d.Code == codeNoSuchAccount {
		return nil
	}
	return e
}

type Identity struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Username string `json:"username"`
}

func (c *Client) ListIdentities(ctx context.Context, tok string) ([]Identity, error) {
	var r []Identity
	if e := c.do(ctx, "GET", "/api/identities/list", tok, nil, &r); e != nil {
		return nil, e
	}
	return r, nil
}

// CreateIdentity stores the per-VM private key as a personal ssh identity.
func (c *Client) CreateIdentity(ctx context.Context, tok, name, login string, key Secret) (int64, error) {
	var r struct {
		ID int64 `json:"id"`
	}
	in := map[string]string{"name": name, "username": login, "type": "ssh", "sshKey": key.Reveal()}
	if e := c.do(ctx, "PUT", "/api/identities", tok, in, &r); e != nil {
		return 0, e
	}
	if r.ID <= 0 {
		return 0, errors.New("nexterm: identity id missing")
	}
	return r.ID, nil
}

func (c *Client) DeleteIdentity(ctx context.Context, tok string, id int64) error {
	return c.do(ctx, "DELETE", "/api/identities/"+strconv.FormatInt(id, 10), tok, nil, nil)
}

type Entry struct {
	ID         int64
	Name       string
	IP, Port   string
	Protocol   string
	Identities []int64
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}

func entryFrom(m map[string]any) (Entry, bool) {
	id, ok := m["id"].(float64)
	cfg, ok2 := m["config"].(map[string]any)
	if !ok || !ok2 || id <= 0 {
		return Entry{}, false
	}
	e := Entry{ID: int64(id), Name: str(m["name"]), IP: str(cfg["ip"]), Port: str(cfg["port"]), Protocol: str(cfg["protocol"])}
	if ids, ok := m["identities"].([]any); ok {
		for _, i := range ids {
			if f, ok := i.(float64); ok {
				e.Identities = append(e.Identities, int64(f))
			}
		}
	}
	return e, true
}

// GetEntry reads one entry back (used to verify what was stored).
func (c *Client) GetEntry(ctx context.Context, tok string, id int64) (Entry, error) {
	var m map[string]any
	if e := c.do(ctx, "GET", "/api/entries/"+strconv.FormatInt(id, 10), tok, nil, &m); e != nil {
		return Entry{}, e
	}
	e, ok := entryFrom(m)
	if !ok {
		return Entry{}, errors.New("nexterm: invalid entry readback")
	}
	return e, nil
}

// FindEntry walks the (folder-nested) entry list for an exact name.
func (c *Client) FindEntry(ctx context.Context, tok, name string) (int64, error) {
	var v any
	if e := c.do(ctx, "GET", "/api/entries/list", tok, nil, &v); e != nil {
		return 0, e
	}
	var found []int64
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case []any:
			for _, i := range x {
				walk(i)
			}
		case map[string]any:
			if e, ok := entryFrom(x); ok && e.Name == name {
				found = append(found, e.ID)
			}
			if sub, ok := x["entries"]; ok {
				walk(sub)
			}
		}
	}
	walk(v)
	if len(found) > 1 {
		return 0, errors.New("nexterm: ambiguous entry readback")
	}
	if len(found) == 0 {
		return 0, nil
	}
	return found[0], nil
}

func (c *Client) CreateEntry(ctx context.Context, tok, name, host string, port int, identity int64) (int64, error) {
	var r struct {
		ID int64 `json:"id"`
	}
	in := map[string]any{"name": name, "type": "server", "identities": []int64{identity},
		"config": map[string]any{"protocol": "ssh", "ip": host, "port": port}}
	if e := c.do(ctx, "PUT", "/api/entries", tok, in, &r); e != nil {
		return 0, e
	}
	if r.ID <= 0 {
		return 0, errors.New("nexterm: entry id missing")
	}
	return r.ID, nil
}

func (c *Client) DeleteEntry(ctx context.Context, tok string, id int64) error {
	return c.do(ctx, "DELETE", "/api/entries/"+strconv.FormatInt(id, 10), tok, nil, nil)
}
