package nextermaccess

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
)

// Config is the optional API "nexterm" section. Disabled by default.
type Config struct {
	Enabled bool `json:"enabled"`
	// URL is the stock Nexterm origin; it is also the link handed to owners.
	URL string `json:"url,omitempty"`
	// AdminCredentialsFile: private 0600 JSON {"username":"..","password":".."}
	// of an administrator used only to create/delete the per-VM accounts.
	AdminCredentialsFile string `json:"admin_credentials_file,omitempty"`
	// StateFile: durable owner+VM bindings (0600 file in a 0700 directory).
	StateFile string `json:"state_file,omitempty"`
}

// Validate is strict only when enabled; disabled must carry no settings.
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	if !c.Enabled {
		if c.URL != "" || c.AdminCredentialsFile != "" || c.StateFile != "" {
			return errors.New("nexterm settings present but nexterm is disabled")
		}
		return nil
	}
	if e := ValidateURL(c.URL); e != nil {
		return e
	}
	for _, p := range []string{c.AdminCredentialsFile, c.StateFile} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return errors.New("nexterm private paths must be absolute and clean")
		}
	}
	if c.AdminCredentialsFile == c.StateFile {
		return errors.New("nexterm credential and state paths must differ")
	}
	return nil
}

// VMAPI is the subset of controlclient.Client used here; every call acts as the
// VM owner, so the control API itself proves ownership.
type VMAPI interface {
	VM(ctx context.Context, u int64, id string) (controlclient.VM, error)
	ListVMs(ctx context.Context, u int64) ([]controlclient.VM, error)
	SSH(ctx context.Context, u int64, id string) (controlclient.SSH, error)
	SSHKey(ctx context.Context, u int64, id string) (controlclient.GeneratedKey, error)
}

type Status string

const (
	Ready       Status = "ready"
	Pending     Status = "pending"
	Unsupported Status = "unsupported"
)

// Result is what the bot may tell the owner. Password is redacting.
type Result struct {
	Status   Status
	VMID     string
	Owner    int64
	URL      string
	Username string
	Password Secret
	Reason   string
}

var (
	ErrNotOwner   = errors.New("nexterm binding belongs to another owner")
	ErrVMInactive = errors.New("vm is expired or being deleted")
)

// vmGone: the VM no longer deserves an account (TTL passed or being removed).
func vmGone(v controlclient.VM, now time.Time) bool {
	if v.ExpiresAt != nil && !now.Before(*v.ExpiresAt) {
		return true
	}
	for _, x := range []string{v.State, v.Phase} {
		switch strings.ToLower(x) {
		case "deleting", "deleted", "expired", "expiring":
			return true
		}
	}
	return false
}

// Track durably records the intent to provision for a just-accepted create, so
// retries survive a restart even if the create operation is still running. It
// performs no Nexterm call and needs no credentials yet.
func (m *Manager) Track(owner int64, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner <= 0 || vmID == "" {
		return errors.New("invalid owner or vm")
	}
	if r, ok := m.store.Get(vmID); ok {
		if r.Owner != owner {
			return ErrNotOwner
		}
		return nil
	}
	return m.store.Put(Record{VMID: vmID, Owner: owner, Stage: StagePlanned, Reason: "tracked", Updated: m.now()})
}

type Manager struct {
	mu            sync.Mutex
	nx            *Client
	store         *Store
	cfg           Config
	api           VMAPI
	now           func() time.Time
	next          map[string]time.Time // in-memory retry backoff
	fails         map[string]int
	lastReconcile time.Time
}

// MaxRetryPerTick and MaxDeletePerTick bound background work.
const (
	MaxRetryPerTick  = 3
	MaxDeletePerTick = 5
	ReconcileEvery   = 5 * time.Minute
)

func NewManager(cfg Config, nx *Client, store *Store, api VMAPI) (*Manager, error) {
	if !cfg.Enabled || nx == nil || store == nil || api == nil {
		return nil, errors.New("invalid nexterm manager configuration")
	}
	return &Manager{nx: nx, store: store, cfg: cfg, api: api, now: time.Now, next: map[string]time.Time{}, fails: map[string]int{}}, nil
}

type adminCreds struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (m *Manager) admin() (string, Secret, error) {
	b, e := ReadPrivate(m.cfg.AdminCredentialsFile)
	if e != nil {
		return "", "", errors.New("cannot read nexterm admin credentials")
	}
	var a adminCreds
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&a) != nil || d.Decode(new(any)) != io.EOF || a.Username == "" || a.Password == "" {
		return "", "", errors.New("invalid nexterm admin credentials")
	}
	return a.Username, Secret(a.Password), nil
}

const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"

func randomString(n int, alphabet string) (string, error) {
	out := make([]byte, 0, n)
	for len(out) < n {
		var b [32]byte
		if _, e := rand.Read(b[:]); e != nil {
			return "", e
		}
		for _, x := range b {
			if int(x) < 256-256%len(alphabet) && len(out) < n {
				out = append(out, alphabet[int(x)%len(alphabet)])
			}
		}
	}
	return string(out), nil
}

// newCredentials: "fx"+10 random chars (12, within Nexterm's 3..15 alphanumeric
// rule; never derived from a Telegram name) and a 28-char password.
func newCredentials() (string, string, error) {
	u, e := randomString(10, alnum)
	if e != nil {
		return "", "", e
	}
	p, e := randomString(28, alnum+"ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	return "fx" + u, p, e
}

func result(r Record, url string) Result {
	switch r.Stage {
	case StageReady:
		return Result{Status: Ready, VMID: r.VMID, Owner: r.Owner, URL: url, Username: r.Username, Password: Secret(r.Password)}
	case StageUnsupported:
		return Result{Status: Unsupported, VMID: r.VMID, Owner: r.Owner, Reason: r.Reason}
	}
	return Result{Status: Pending, VMID: r.VMID, Owner: r.Owner, Reason: r.Reason}
}

func isAPI(e error, code string) bool {
	var a *controlclient.APIError
	return errors.As(e, &a) && a.Code == code
}

// Ensure provisions (or resumes) the single binding for owner+VM. A Nexterm or
// transient API failure yields a Pending result, not an error; only a store
// failure or an ownership/identity mismatch is an error.
func (m *Manager) Ensure(ctx context.Context, owner int64, vmID string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensure(ctx, owner, vmID)
}

func (m *Manager) fail(rec Record, reason string) (Result, error) {
	rec.Reason = reason
	rec.Updated = m.now()
	if rec.Stage == "" {
		rec.Stage = StagePlanned
	}
	if e := m.store.Put(rec); e != nil {
		return Result{}, e
	}
	m.fails[rec.VMID]++
	n := m.fails[rec.VMID]
	d := 30 * time.Second << min(n-1, 5)
	m.next[rec.VMID] = m.now().Add(d)
	return result(rec, m.cfg.URL), nil
}

func (m *Manager) ensure(ctx context.Context, owner int64, vmID string) (Result, error) {
	if owner <= 0 || vmID == "" {
		return Result{}, errors.New("invalid owner or vm")
	}
	rec, have := m.store.Get(vmID)
	if have && rec.Owner != owner {
		return Result{}, ErrNotOwner
	}
	// Every call (including a ready binding) re-proves through the control API
	// that the VM still exists, belongs to this owner and has not expired.
	vm, e := m.api.VM(ctx, owner, vmID)
	if e != nil || vm.ID != vmID {
		if isAPI(e, "not_found") || isAPI(e, "forbidden") {
			return Result{}, ErrNotOwner
		}
		if have && rec.Stage != StagePlanned {
			return Result{Status: Pending, VMID: vmID, Owner: owner, Reason: "vm_api_unavailable"}, nil
		}
		if !have {
			rec = Record{VMID: vmID, Owner: owner}
		}
		return m.fail(rec, "vm_api_unavailable")
	}
	if vmGone(vm, m.now()) {
		return Result{}, ErrVMInactive
	}
	if have && (rec.Stage == StageReady || rec.Stage == StageUnsupported) {
		return result(rec, m.cfg.URL), nil
	}
	if !have {
		rec = Record{VMID: vmID, Owner: owner}
	}
	ssh, e := m.api.SSH(ctx, owner, vmID)
	if e != nil || ssh.Host == "" || ssh.Port < 1 || ssh.Port > 65535 || ssh.User == "" || strings.ContainsAny(ssh.Host+ssh.User, " \r\n\t") {
		return m.fail(rec, "ssh_endpoint_unavailable")
	}
	var key Secret
	if rec.IdentityID == 0 {
		k, e := m.api.SSHKey(ctx, owner, vmID)
		if isAPI(e, "key_not_generated") {
			rec.Stage, rec.Reason, rec.Updated = StageUnsupported, "user_supplied_ssh_key", m.now()
			rec.Username, rec.Password = "", ""
			if pe := m.store.Put(rec); pe != nil {
				return Result{}, pe
			}
			return result(rec, m.cfg.URL), nil
		}
		valid := e == nil && k.VMID == vmID && strings.HasPrefix(strings.TrimSpace(k.PrivateKey), "-----BEGIN") && len(k.PrivateKey) < 16<<10 && !strings.ContainsRune(k.PrivateKey, 0)
		if !valid {
			return m.fail(rec, "ssh_key_unavailable")
		}
		key = Secret(k.PrivateKey)
	}
	if rec.Username == "" || rec.Password == "" {
		u, p, e := newCredentials()
		if e != nil {
			return Result{}, errors.New("cannot generate credentials")
		}
		rec.Username, rec.Password = u, p
	}
	rec.Stage, rec.Updated = StagePlanned, m.now()
	if e := m.store.Put(rec); e != nil { // credentials are durable BEFORE any Nexterm write
		return Result{}, e
	}
	if e := m.provision(ctx, &rec, vm, ssh, key); e != nil {
		return m.fail(rec, reasonOf(e))
	}
	rec.Stage, rec.Reason, rec.Updated = StageReady, "", m.now()
	if e := m.store.Put(rec); e != nil {
		return Result{}, e
	}
	delete(m.fails, vmID)
	delete(m.next, vmID)
	return result(rec, m.cfg.URL), nil
}

func reasonOf(e error) string {
	var d *DomainError
	var h *HTTPError
	switch {
	case errors.As(e, &d):
		return "nexterm_rejected"
	case errors.As(e, &h):
		return "nexterm_http_error"
	case errors.Is(e, context.Canceled), errors.Is(e, context.DeadlineExceeded):
		return "timeout"
	case strings.HasPrefix(e.Error(), "nexterm:"):
		return "nexterm_unavailable"
	}
	return "provisioning_failed"
}

func (m *Manager) bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// session logs in as the admin and returns a logout func that always runs on a
// fresh context so a canceled request cannot leak the admin session.
func (m *Manager) adminSession(ctx context.Context) (string, func(), error) {
	u, p, e := m.admin()
	if e != nil {
		return "", nil, e
	}
	tok, e := m.nx.Login(ctx, u, p)
	if e != nil {
		return "", nil, e
	}
	return tok, m.logout(tok), nil
}

func (m *Manager) logout(tok string) func() {
	return func() {
		c, cancel := m.bg()
		defer cancel()
		_ = m.nx.CloseSession(c, tok)
	}
}

func (m *Manager) provision(ctx context.Context, rec *Record, vm controlclient.VM, ssh controlclient.SSH, key Secret) error {
	admin, done, e := m.adminSession(ctx)
	if e != nil {
		return e
	}
	defer done()
	id, e := m.nx.FindUser(ctx, admin, rec.Username)
	if e != nil {
		return e
	}
	created := false
	if id == 0 {
		if e = m.nx.CreateUser(ctx, admin, rec.Username, Secret(rec.Password)); e != nil {
			return e
		}
		created = true
		if id, e = m.nx.FindUser(ctx, admin, rec.Username); e != nil {
			return e
		}
		if id == 0 {
			return errors.New("nexterm: created account not found")
		}
	}
	if rec.UserID != 0 && rec.UserID != id {
		return errors.New("nexterm: account id changed")
	}
	if rec.UserID != id {
		rec.UserID = id
		if e = m.store.Put(*rec); e != nil {
			return e
		}
	}
	if !created {
		// An account with our random name already existed (crash after create):
		// prove the stored password is the one that account has.
		tok, e := m.nx.Login(ctx, rec.Username, Secret(rec.Password))
		if e != nil {
			return e
		}
		m.logout(tok)()
	}
	sess, e := m.nx.Impersonate(ctx, admin, id)
	if e != nil {
		return e
	}
	defer m.logout(sess)()
	me, e := m.nx.Me(ctx, sess)
	if e != nil {
		return e
	}
	if me.ID != id || me.Username != rec.Username || me.IsAdmin {
		return errors.New("nexterm: account readback mismatch")
	}
	name := "vm-" + vm.Name
	if rec.IdentityID == 0 {
		ids, e := m.nx.ListIdentities(ctx, sess)
		if e != nil {
			return e
		}
		for _, i := range ids {
			if i.Name == name {
				rec.IdentityID = i.ID
			}
		}
		if rec.IdentityID == 0 {
			if key == "" {
				return errors.New("nexterm: key unavailable")
			}
			if rec.IdentityID, e = m.nx.CreateIdentity(ctx, sess, name, ssh.User, key); e != nil {
				return e
			}
		}
		if e = m.store.Put(*rec); e != nil {
			return e
		}
	}
	if rec.EntryID == 0 {
		if rec.EntryID, e = m.nx.FindEntry(ctx, sess, name); e != nil {
			return e
		}
		if rec.EntryID == 0 {
			if rec.EntryID, e = m.nx.CreateEntry(ctx, sess, name, ssh.Host, ssh.Port, rec.IdentityID); e != nil {
				return e
			}
		}
		if e = m.store.Put(*rec); e != nil {
			return e
		}
	}
	en, e := m.nx.GetEntry(ctx, sess, rec.EntryID)
	if e != nil {
		return e
	}
	okID := false
	for _, i := range en.Identities {
		okID = okID || i == rec.IdentityID
	}
	if en.Protocol != "ssh" || en.IP != ssh.Host || en.Port != itoa(ssh.Port) || !okID {
		return errors.New("nexterm: entry readback mismatch")
	}
	return nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Undelivered lists ready bindings not yet sent, re-validating each VM (active,
// owned, unexpired) through the control API first; a stale binding is skipped.
func (m *Manager) Undelivered(ctx context.Context) []Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Result
	for _, r := range m.store.All() {
		if r.Stage != StageReady || r.Delivered {
			continue
		}
		vm, e := m.api.VM(ctx, r.Owner, r.VMID)
		if e != nil || vm.ID != r.VMID || vmGone(vm, m.now()) {
			continue
		}
		out = append(out, result(r, m.cfg.URL))
	}
	return out
}

var errLinkMismatch = errors.New("nexterm: link session mismatch")

// PrepareLink returns the owner's auto-login link
// <origin>/servers?token=USER_SESSION&connectId=ENTRY_ID for a ready binding.
// It re-proves owner+VM liveness through the control API, reuses the stored
// per-user link session while Nexterm still accepts it for the exact recorded
// account (non-admin, ID and username match, entry readable) and otherwise
// creates one session by admin impersonation of that account, validates it and
// persists it BEFORE returning. The admin session is always closed; a stored
// token that is rejected-but-alive is closed individually (never POST
// /api/auth/logout). Transient failures never mint extra sessions. Errors never
// contain the token; callers fall back to URL/login/password.
func (m *Manager) PrepareLink(ctx context.Context, owner int64, vmID string) (Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.store.Get(vmID)
	if !ok || owner <= 0 || rec.Owner != owner {
		return "", ErrNotOwner
	}
	if rec.Stage != StageReady || rec.UserID <= 0 || rec.EntryID <= 0 || rec.Username == "" {
		return "", errors.New("nexterm binding not ready")
	}
	vm, e := m.api.VM(ctx, owner, vmID)
	if e != nil || vm.ID != vmID {
		if isAPI(e, "not_found") || isAPI(e, "forbidden") {
			return "", ErrNotOwner
		}
		return "", errors.New("vm api unavailable")
	}
	if vmGone(vm, m.now()) {
		return "", ErrVMInactive
	}
	valid := func(tok string) error {
		me, e := m.nx.Me(ctx, tok)
		if e != nil {
			return e
		}
		if me.ID != rec.UserID || me.Username != rec.Username || me.IsAdmin {
			return errLinkMismatch
		}
		_, e = m.nx.GetEntry(ctx, tok, rec.EntryID)
		return e
	}
	if rec.LinkToken != "" {
		e := valid(rec.LinkToken)
		if e == nil {
			return m.linkURL(rec.LinkToken, rec.EntryID), nil
		}
		var h *HTTPError
		switch {
		case errors.As(e, &h) && h.Status == 401: // already dead: nothing to close
		case errors.Is(e, errLinkMismatch):
			_ = m.nx.CloseSession(ctx, rec.LinkToken)
		default:
			return "", errors.New("nexterm link session check failed")
		}
		rec.LinkToken = ""
		if pe := m.store.Put(rec); pe != nil {
			return "", pe
		}
	}
	admin, done, e := m.adminSession(ctx)
	if e != nil {
		return "", errors.New("nexterm link unavailable")
	}
	defer done()
	tok, e := m.nx.Impersonate(ctx, admin, rec.UserID)
	if e != nil {
		return "", errors.New("nexterm link unavailable")
	}
	if valid(tok) != nil {
		m.logout(tok)()
		return "", errors.New("nexterm link session refused")
	}
	rec.LinkToken = tok
	if pe := m.store.Put(rec); pe != nil {
		m.logout(tok)()
		return "", pe
	}
	return m.linkURL(tok, rec.EntryID), nil
}

func (m *Manager) linkURL(tok string, entry int64) Secret {
	return Secret(strings.TrimRight(m.cfg.URL, "/") + "/servers?token=" + url.QueryEscape(tok) + "&connectId=" + strconv.FormatInt(entry, 10) + "&connectFtpId=" + strconv.FormatInt(entry, 10))
}

// ErrUnsupported: the VM uses an owner-supplied SSH key, so no browser entry
// can be created (raw SSH is unaffected). ErrPending: provisioning is deferred.
var (
	ErrUnsupported = errors.New("nexterm unsupported for this vm")
	ErrPending     = errors.New("nexterm provisioning pending")
)

// BrowserLink is the single unified entry point: it provisions (or resumes) the
// owner's per-VM account and returns the browser SSH link. The per-VM account
// and ordinary link session are reused; nothing here returns the admin token,
// the private key or the account password.
func (m *Manager) BrowserLink(ctx context.Context, owner int64, vmID string) (Secret, error) {
	r, e := m.Ensure(ctx, owner, vmID)
	if e != nil {
		return "", e
	}
	switch r.Status {
	case Unsupported:
		return "", ErrUnsupported
	case Ready:
		return m.PrepareLink(ctx, owner, vmID)
	}
	return "", ErrPending
}

// CheckCredentials validates the admin credential file without any network call.
func (m *Manager) CheckCredentials() error {
	_, _, e := m.admin()
	return e
}

// BackgroundInterval is how often Run retries pending bindings and reconciles.
const BackgroundInterval = 30 * time.Second

// Run drives retry and TTL/delete cleanup until ctx ends. Reconcile is
// interval-gated internally; the caller must wait for Run to return before it
// closes the store.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(BackgroundInterval)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		// Run once immediately on startup, then bound each batch so an
		// unavailable Nexterm cannot hold the manager across multiple ticks.
		batch, cancel := context.WithTimeout(ctx, 25*time.Second)
		m.Reconcile(batch)
		m.Retry(batch)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// MarkDelivered is called after Telegram accepted the private message.
func (m *Manager) MarkDelivered(vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.store.Get(vmID)
	if !ok {
		return nil
	}
	r.Delivered = true
	return m.store.Put(r)
}

// Retry resumes at most MaxRetryPerTick due pending bindings (with exponential
// backoff). It never deletes anything.
func (m *Manager) Retry(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.store.All() {
		if r.Stage != StagePlanned || n >= MaxRetryPerTick || ctx.Err() != nil {
			continue
		}
		if t, ok := m.next[r.VMID]; ok && m.now().Before(t) {
			continue
		}
		n++
		_, _ = m.ensure(ctx, r.Owner, r.VMID)
	}
}

// Reconcile removes the accounts of VMs that are provably gone (deleted by the
// bot, API or TTL). A VM counts as gone only when the owner's list succeeded
// without it AND a direct lookup answered not_found; any other error keeps the
// binding. At most MaxDeletePerTick are removed per call, at most every
// ReconcileEvery. Only accounts recorded in our own state are ever touched.
func (m *Manager) Reconcile(ctx context.Context) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.now().Sub(m.lastReconcile) < ReconcileEvery {
		return 0
	}
	m.lastReconcile = m.now()
	return m.reconcile(ctx)
}

func (m *Manager) reconcile(ctx context.Context) int {
	live := map[int64]map[string]controlclient.VM{}
	deleted := 0
	for _, r := range m.store.All() {
		if deleted >= MaxDeletePerTick || ctx.Err() != nil {
			break
		}
		set, seen := live[r.Owner]
		if !seen {
			vms, e := m.api.ListVMs(ctx, r.Owner)
			if e == nil {
				set = map[string]controlclient.VM{}
				for _, v := range vms {
					set[v.ID] = v
				}
			}
			live[r.Owner] = set
		}
		if set == nil {
			continue
		}
		if v, listed := set[r.VMID]; listed {
			if !vmGone(v, m.now()) {
				continue
			}
		} else if _, e := m.api.VM(ctx, r.Owner, r.VMID); !isAPI(e, "not_found") {
			continue
		}
		if m.cleanup(ctx, r) == nil {
			deleted++
		}
	}
	return deleted
}

// cleanup fails closed: an account is deleted only after it is proven to be the
// one we created (exact recorded ID + username + ordinary-account readback, or,
// when the ID was never recorded, a successful login with the stored password
// followed by the same readback). Entry/identity are removed first (best effort)
// and the binding is forgotten only after the account is read back as absent.
func (m *Manager) cleanup(ctx context.Context, r Record) error {
	if r.Username == "" {
		return m.store.Delete(r.VMID)
	}
	admin, done, e := m.adminSession(ctx)
	if e != nil {
		return e
	}
	defer done()
	id, e := m.nx.FindUser(ctx, admin, r.Username)
	if e != nil {
		return e
	}
	if id != 0 {
		var sess string
		if r.UserID != 0 {
			if r.UserID != id {
				return errors.New("nexterm: refusing to delete account with unexpected id")
			}
			if sess, e = m.nx.Impersonate(ctx, admin, id); e != nil {
				return e
			}
		} else if r.Password != "" {
			if sess, e = m.nx.Login(ctx, r.Username, Secret(r.Password)); e != nil {
				return e
			}
		} else {
			return errors.New("nexterm: cannot prove account ownership")
		}
		me, e := m.nx.Me(ctx, sess)
		if e != nil || me.ID != id || me.Username != r.Username || me.IsAdmin {
			m.logout(sess)()
			return errors.New("nexterm: account readback refused deletion")
		}
		if r.LinkToken != "" && r.LinkToken != sess {
			_ = m.nx.CloseSession(ctx, r.LinkToken) // only that token, never a global logout
		}
		if r.EntryID != 0 {
			_ = m.nx.DeleteEntry(ctx, sess, r.EntryID)
		}
		if r.IdentityID != 0 {
			_ = m.nx.DeleteIdentity(ctx, sess, r.IdentityID)
		}
		m.logout(sess)()
		if e = m.nx.DeleteUser(ctx, admin, id); e != nil {
			return e
		}
	}
	if left, e := m.nx.FindUser(ctx, admin, r.Username); e != nil || left != 0 {
		return errors.New("nexterm: account still present after delete")
	}
	return m.store.Delete(r.VMID)
}

// ReconcileNow is Reconcile without the interval gate (used after a bot /delete).
func (m *Manager) ReconcileNow(ctx context.Context) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reconcile(ctx)
}
