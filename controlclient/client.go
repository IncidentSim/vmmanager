// Package controlclient is the HTTP-only v1 control API SDK. It has no VM
// controller, DBOS, or gRPC dependencies. Mutations never retry with new keys.
package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponse = 4 << 20

type Resources struct {
	VCPU      uint32 `json:"vcpu"`
	MemoryMiB uint32 `json:"memory_mib"`
}
type VM struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ImageID   string    `json:"image_id"`
	State     string    `json:"state"`
	Phase     string    `json:"phase"`
	Resources Resources `json:"resources"`
	// OwnerError: the VM's durable owner workflow ended abnormally; the VM is
	// retained and commands are refused. Omitted (false) otherwise.
	OwnerError bool `json:"owner_error,omitempty"`
}
type Image struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	// Shared: visible and bootable by every admitted user (common registry).
	Shared bool `json:"shared"`
	// Phase is ready or deleting (a deleting image is not bootable).
	Phase string `json:"phase,omitempty"`
	// CanDelete is the caller's permission (creator, not a system image), not a
	// guarantee that no VM references the image; delete may still return 409.
	CanDelete bool `json:"can_delete"`
}
type Profile struct {
	Name      string `json:"name"`
	VCPU      uint32 `json:"vcpu"`
	MemoryMiB uint32 `json:"memory_mib"`
	DiskMiB   uint32 `json:"disk_mib,omitempty"`
}
type Capabilities struct {
	MaxVMs        int `json:"max_vms"`
	MaxVMsPerUser int `json:"max_vms_per_user"`
	// BackendMaxVMs is the backend's own cap; the API quotas above stay authoritative for owners.
	BackendMaxVMs int `json:"backend_max_vms,omitempty"`
	// Aggregate backend caps across live VMs; zero means not enforced/advertised.
	MaxTotalMemoryMiB uint32    `json:"max_total_memory_mib,omitempty"`
	MaxTotalVCPU      uint32    `json:"max_total_vcpu,omitempty"`
	Profiles          []Profile `json:"profiles"`
}
type SSH struct {
	Host               string          `json:"host"`
	Port               int             `json:"port"`
	User               string          `json:"user"`
	Bastion            json.RawMessage `json:"bastion,omitempty"`
	HostKeyFingerprint string          `json:"host_key_fingerprint"`
	Ready              bool            `json:"ready"`
}
type CreateRequest struct {
	Name         string     `json:"name,omitempty"`
	ImageID      string     `json:"image_id"`
	Resources    *Resources `json:"resources,omitempty"`
	SSHPublicKey string     `json:"ssh_public_key,omitempty"`
}
type Accepted struct {
	OperationID string `json:"operation_id"`
	VMID        string `json:"vm_id,omitempty"`
	ImageID     string `json:"image_id,omitempty"`
}
type Operation struct {
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Kind    string          `json:"kind"`
	VMID    string          `json:"vm_id,omitempty"`
	ImageID string          `json:"image_id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *APIError       `json:"error,omitempty"`
}

// APIError deliberately omits the server's free-form message from Error().
type APIError struct {
	Code   string `json:"code"`
	Status int    `json:"-"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control API: %s (HTTP %d)", safeCode(e.Code), e.Status)
}
func safeCode(s string) string {
	switch s {
	case "not_found", "unauthorized", "forbidden", "invalid_argument", "conflict", "capacity", "unavailable", "internal":
		return s
	}
	return "internal"
}

type Client struct {
	base, token string
	http        *http.Client
	boundUser   int64
	// tokenFor, when set, supplies a user-specific bearer for every request; the
	// client then never sends X-Acting-User-ID (no delegation).
	tokenFor func(userID int64) (string, error)
}

// NewPerUser is for a frontend that signs a separate bearer per user (the API
// derives the principal from the verified token). There is no service
// credential and no X-Acting-User-ID.
func NewPerUser(base string, tokenFor func(userID int64) (string, error), hc *http.Client) (*Client, error) {
	if tokenFor == nil {
		return nil, errors.New("per-user token source required")
	}
	c, err := New(base, "per-user", hc)
	if err != nil {
		return nil, err
	}
	c.token, c.tokenFor = "", tokenFor
	return c, nil
}

// NewUser binds an ordinary user bearer to its configured identity. Unlike New
// (a delegated service client), it never sends X-Acting-User-ID.
func NewUser(base, token string, userID int64, hc *http.Client) (*Client, error) {
	if userID <= 0 {
		return nil, errors.New("invalid user identity")
	}
	c, err := New(base, token, hc)
	if err != nil {
		return nil, err
	}
	c.boundUser = userID
	return c, nil
}

// New permits plaintext only for a literal loopback IP or localhost; redirects
// are never followed, including same-origin redirects. Every request is bounded.
func New(base, token string, hc *http.Client) (*Client, error) {
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("invalid control API URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return nil, errors.New("control API requires HTTPS except loopback")
		}
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("invalid service credential")
	}
	var h http.Client
	if hc != nil {
		h = *hc
	}
	if h.Timeout <= 0 || h.Timeout > 30*time.Second {
		h.Timeout = 30 * time.Second
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &h}, nil
}
func validKey(s string) bool {
	if len(s) < 1 || len(s) > 200 {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func (c *Client) request(ctx context.Context, user int64, method, path, key string, in, out any) error {
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return errors.New("control API: invalid resource ID")
		}
	}
	if user <= 0 || (c.boundUser != 0 && c.boundUser != user) {
		return errors.New("invalid acting user")
	}
	if method != "GET" && !validKey(key) {
		return errors.New("invalid idempotency key")
	}
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return errors.New("control API: encode failed")
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if e != nil {
		return errors.New("control API: invalid request")
	}
	token := c.token
	if c.tokenFor != nil {
		var terr error
		if token, terr = c.tokenFor(user); terr != nil || token == "" || strings.ContainsAny(token, "\r\n ") {
			return errors.New("control API: no user credential")
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if c.boundUser == 0 && c.tokenFor == nil {
		req.Header.Set("X-Acting-User-ID", strconv.FormatInt(user, 10))
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, e := c.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("control API: transport failed")
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if e != nil || len(b) > maxResponse {
		return errors.New("control API: invalid response size")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var env struct {
			Error APIError `json:"error"`
		}
		_ = json.Unmarshal(b, &env)
		env.Error.Code = safeCode(env.Error.Code)
		env.Error.Status = resp.StatusCode
		return &env.Error
	}
	expected := 200
	if method != "GET" {
		expected = 202
	}
	if resp.StatusCode != expected {
		return errors.New("control API: unexpected status")
	}
	if out != nil && (len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' || json.Unmarshal(b, out) != nil) {
		return errors.New("control API: invalid response")
	}
	return nil
}
func vmPath(id string) string { return "/v1/vms/" + url.PathEscape(id) }
func (c *Client) ListVMs(ctx context.Context, u int64) ([]VM, error) {
	var v struct {
		VMs []VM `json:"vms"`
	}
	e := c.request(ctx, u, "GET", "/v1/vms", "", nil, &v)
	return v.VMs, e
}
func (c *Client) VM(ctx context.Context, u int64, id string) (VM, error) {
	var v VM
	e := c.request(ctx, u, "GET", vmPath(id), "", nil, &v)
	return v, e
}
func (c *Client) Create(ctx context.Context, u int64, key string, r CreateRequest) (Accepted, error) {
	var a Accepted
	e := c.request(ctx, u, "POST", "/v1/vms", key, r, &a)
	if e == nil && (a.OperationID == "" || a.VMID == "") {
		e = errors.New("control API: invalid accepted envelope")
	}
	return a, e
}
func (c *Client) Stop(ctx context.Context, u int64, id, key string) (Accepted, error) {
	return c.mutate(ctx, u, "POST", vmPath(id)+"/stop", key, struct{}{})
}
func (c *Client) Delete(ctx context.Context, u int64, id, key string) (Accepted, error) {
	return c.mutate(ctx, u, "DELETE", vmPath(id), key, nil)
}
func (c *Client) Commit(ctx context.Context, u int64, id, key, name string) (Accepted, error) {
	return c.mutate(ctx, u, "POST", vmPath(id)+"/commit", key, struct {
		Name string `json:"name"`
	}{name})
}
func (c *Client) Terminal(ctx context.Context, u int64, id, key string) (Accepted, error) {
	return c.mutate(ctx, u, "POST", vmPath(id)+"/terminal", key, struct{}{})
}
func (c *Client) mutate(ctx context.Context, u int64, method, path, key string, in any) (Accepted, error) {
	var a Accepted
	e := c.request(ctx, u, method, path, key, in, &a)
	if e == nil && a.OperationID == "" {
		e = errors.New("control API: invalid accepted envelope")
	}
	return a, e
}
func (c *Client) SSH(ctx context.Context, u int64, id string) (SSH, error) {
	var v SSH
	e := c.request(ctx, u, "GET", vmPath(id)+"/ssh", "", nil, &v)
	return v, e
}
func (c *Client) Images(ctx context.Context, u int64) ([]Image, error) {
	var v struct {
		Images []Image `json:"images"`
	}
	e := c.request(ctx, u, "GET", "/v1/images", "", nil, &v)
	return v.Images, e
}
func (c *Client) Image(ctx context.Context, u int64, id string) (Image, error) {
	var v Image
	e := c.request(ctx, u, "GET", "/v1/images/"+url.PathEscape(id), "", nil, &v)
	return v, e
}
func (c *Client) DeleteImage(ctx context.Context, u int64, id, key string) (Accepted, error) {
	return c.mutate(ctx, u, "DELETE", "/v1/images/"+url.PathEscape(id), key, nil)
}
func (c *Client) Capabilities(ctx context.Context, u int64) (Capabilities, error) {
	var v Capabilities
	e := c.request(ctx, u, "GET", "/v1/capabilities", "", nil, &v)
	return v, e
}
func (c *Client) Operation(ctx context.Context, u int64, id string) (Operation, error) {
	var v Operation
	e := c.request(ctx, u, "GET", "/v1/operations/"+url.PathEscape(id), "", nil, &v)
	if e == nil && (v.ID != id || (v.Status != "pending" && v.Status != "running" && v.Status != "succeeded" && v.Status != "failed")) {
		e = errors.New("control API: invalid operation envelope")
	}
	return v, e
}

// Wait returns the last observed operation on budget expiry, never inventing a
// failed durable result. An absent caller deadline is capped at one minute.
func (c *Client) Wait(ctx context.Context, u int64, id string, interval time.Duration) (Operation, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if interval <= 0 {
		interval = time.Second
	}
	var last Operation
	for {
		v, e := c.Operation(ctx, u, id)
		if e != nil {
			return last, e
		}
		last = v
		switch v.Status {
		case "succeeded":
			return v, nil
		case "failed":
			if v.Error != nil {
				return v, v.Error
			}
			return v, errors.New("control API: operation failed")
		case "pending", "running":
		default:
			return last, errors.New("control API: invalid operation status")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, ctx.Err()
		case <-timer.C:
		}
	}
}
