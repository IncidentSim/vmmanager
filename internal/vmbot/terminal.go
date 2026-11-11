package vmbot

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// ManagerConfig configures the terminal backend and gateway.
type ManagerConfig struct {
	// Credentials selects dedicated per-VM terminal identities except for an
	// explicit image-preinstalled per-VM policy. Missing keys never fall back.
	Credentials                   TerminalCredentials
	KeyRuntimeDir                 string
	TTYDPath, SSHPath, SSHKeyFile string
	StateDir                      string // 0700; holds ttyd sockets and per-VM known_hosts
	PublicOrigin                  string // exact scheme://host[:port] allowed in Host/Origin
	MaxClients                    int    // WebSocket clients per VM
	MaxSessions                   int    // concurrent VM terminals (ttyd processes)
	UseBastion                    bool
	StartTimeout                  time.Duration
}

var (
	tokenPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
	sshUserPattern   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	proxyJumpPattern = regexp.MustCompile(`^([a-z_][a-z0-9_-]{0,31}@)?[A-Za-z0-9][A-Za-z0-9.-]{0,252}(:[0-9]{1,5})?$`)
)

// BuildSSHArgs returns the complete, fixed argv (program first) that ttyd
// executes for a guest. No value comes from a user: the address must parse as an
// IP and the user and optional jump host must match conservative patterns, so
// nothing can start with '-' or smuggle an option.
func BuildSSHArgs(cfg ManagerConfig, t TerminalTarget, knownHosts string) ([]string, error) {
	if err := vmlib.ValidateID("vm", t.VMID); err != nil {
		return nil, fmt.Errorf("terminal target: %w", err)
	}
	ip := net.ParseIP(t.Host)
	if ip == nil {
		return nil, errors.New("terminal target: guest address is not an IP")
	}
	if !sshUserPattern.MatchString(t.User) {
		return nil, errors.New("terminal target: unsafe ssh user")
	}
	bastion := cfg.UseBastion && t.ProxyJump != ""
	if bastion && !proxyJumpPattern.MatchString(t.ProxyJump) {
		return nil, errors.New("terminal target: unsafe bastion")
	}
	if cfg.SSHPath == "" || strings.HasPrefix(cfg.SSHPath, "-") || !filepath.IsAbs(cfg.SSHKeyFile) {
		return nil, errors.New("terminal config: ssh path/key")
	}
	a := []string{cfg.SSHPath, "-F", "/dev/null"}
	opt := func(kv string) { a = append(a, "-o", kv) }
	for _, kv := range []string{
		"BatchMode=yes", "IdentitiesOnly=yes", "IdentityAgent=none",
		"PasswordAuthentication=no", "KbdInteractiveAuthentication=no", "NumberOfPasswordPrompts=0",
		"ForwardAgent=no", "ForwardX11=no", "ForwardX11Trusted=no", "ClearAllForwardings=yes",
		"PermitLocalCommand=no", "ControlMaster=no", "ControlPath=none",
		"StrictHostKeyChecking=accept-new", "UserKnownHostsFile=" + knownHosts, "GlobalKnownHostsFile=/dev/null",
		"UpdateHostKeys=no", "HashKnownHosts=no",
		"ConnectTimeout=10", "ServerAliveInterval=30", "ServerAliveCountMax=3", "LogLevel=ERROR", "RequestTTY=yes",
	} {
		opt(kv)
	}
	if bastion {
		a = append(a, "-J", t.ProxyJump)
	} else {
		opt("ProxyCommand=none")
	}
	a = append(a, "-i", cfg.SSHKeyFile, "-p", "22", t.User+"@"+ip.String())
	return a, nil
}

// Manager runs one ttyd per VM (bounded by MaxSessions) and is also the HTTP
// gateway handler. Sessions are independent: revoking one never touches another's
// processes, sockets, connections or link.
type Manager struct {
	cfg    ManagerConfig
	origin *url.URL

	mu    sync.Mutex          // guards the maps, slot states and closed only; never held across slow work
	sess  map[string]*session // published (routable) sessions only
	slots map[string]*slot    // every VM that starts, runs or is still being cleaned up
	// pending is the subset of slots whose Start is still building the backend
	// (it leaves pending the moment the slot is published or starts terminating,
	// but stays in slots until cleanup has fully finished).
	pending map[string]*slot
	closed  bool
}

type slotState int

const (
	slotStarting    slotState = iota // a Start goroutine owns the half-built backend
	slotActive                       // published in sess
	slotTerminating                  // one owner is killing processes / removing files
)

// slot is the per-VM reservation. It exists from the first Start until ALL
// cleanup (processes, WebSockets, socket and known_hosts files) has finished, and
// only then is removed and done closed. Until then it keeps counting against
// MaxSessions and keeps its capability token reserved, so neither a replacement
// session nor another VM can be admitted over half-cleaned resources, and
// Revoke/Close can wait on done no matter which goroutine owns the cleanup.
type slot struct {
	releaseKey  func()
	vmID, token string
	state       slotState
	cancel      context.CancelFunc // aborts a start in flight
	ready       chan struct{}      // closed when the start attempt has concluded (published or failing)
	done        chan struct{}      // closed when the slot is gone: all cleanup complete
}

func NewManager(cfg ManagerConfig) (*Manager, error) {
	u, err := url.Parse(cfg.PublicOrigin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("terminal config: bad public origin")
	}
	if cfg.MaxClients < 1 {
		return nil, errors.New("terminal config: max clients")
	}
	if cfg.MaxSessions < 1 || cfg.MaxSessions > MaxVMsLimit || cfg.MaxSessions*cfg.MaxClients > maxGatewayConns {
		return nil, errors.New("terminal config: max sessions out of range, or sessions*clients exceeds the gateway connection bound")
	}
	if !filepath.IsAbs(cfg.StateDir) || strings.ContainsAny(cfg.StateDir, " \t\r\n\"'\\") {
		return nil, errors.New("terminal config: state dir must be an absolute path without spaces or quotes")
	}
	if (cfg.Credentials == nil || cfg.SSHKeyFile != "") && (!filepath.IsAbs(cfg.SSHKeyFile) || strings.ContainsAny(cfg.SSHKeyFile, "\r\n")) {
		return nil, errors.New("terminal config: ssh key path")
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 10 * time.Second
	}
	if cfg.KeyRuntimeDir == "" {
		cfg.KeyRuntimeDir = filepath.Join(cfg.StateDir, "terminal-keys")
	}
	if !filepath.IsAbs(cfg.KeyRuntimeDir) || strings.ContainsAny(cfg.KeyRuntimeDir, "\r\n") {
		return nil, errors.New("terminal config: key runtime directory")
	}
	return &Manager{cfg: cfg, origin: &url.URL{Scheme: u.Scheme, Host: u.Host},
		sess: map[string]*session{}, slots: map[string]*slot{}, pending: map[string]*slot{}}, nil
}

// KnownHostsPath is the per-VM known_hosts file.
func (m *Manager) KnownHostsPath(vmID string) string {
	return filepath.Join(m.cfg.StateDir, "known_hosts", vmID)
}

func (m *Manager) sockPath(vmID string) string {
	return filepath.Join(m.cfg.StateDir, "ttyd-"+vmID+".sock")
}

type session struct {
	authorize   func() bool
	vmID, token string
	sock        string
	cmd         *exec.Cmd
	exited      chan struct{}
	proxy       *httputil.ReverseProxy
	max         int
	ctx         context.Context
	cancel      context.CancelFunc

	mu      sync.Mutex
	revoked bool
	clients int
	conns   map[net.Conn]struct{}
}

// Start launches (or confirms) the ttyd backend for one VM. It is idempotent
// for the same VM and token, refuses a changed token, a token already used by
// another VM and any start beyond MaxSessions, and fails loudly when the backend
// does not come up. Other VMs' terminals are never affected or blocked.
func (m *Manager) Start(ctx context.Context, t TerminalTarget) error {
	if t.authorize != nil && !t.authorize() {
		return ErrNotRunning
	}
	if !tokenPattern.MatchString(t.Token) {
		return errors.New("terminal: malformed capability token")
	}
	if err := vmlib.ValidateID("vm", t.VMID); err != nil {
		return fmt.Errorf("terminal target: %w", err)
	}
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return errors.New("terminal: manager closed")
		}
		// A capability token is reserved by every slot (starting, active or
		// still terminating) until its cleanup is complete.
		for id, o := range m.slots {
			if id != t.VMID && subtle.ConstantTimeCompare([]byte(o.token), []byte(t.Token)) == 1 {
				m.mu.Unlock()
				return errors.New("terminal: capability token already in use")
			}
		}
		if sl := m.slots[t.VMID]; sl != nil {
			switch sl.state {
			case slotTerminating:
				// Never publish a replacement over half-cleaned files/processes.
				m.mu.Unlock()
				return errors.New("terminal: previous terminal of this VM is still shutting down; retry")
			case slotStarting:
				m.mu.Unlock()
				select {
				case <-sl.ready:
					continue
				case <-ctx.Done():
					return errors.New("terminal: startup canceled")
				}
			}
			s := m.sess[t.VMID]
			if subtle.ConstantTimeCompare([]byte(sl.token), []byte(t.Token)) != 1 {
				m.mu.Unlock()
				return errors.New("terminal: this VM's terminal is active with a different capability; revoke it first")
			}
			select {
			case <-s.exited: // backend died: clean it up completely, then start anew
				sl.state = slotTerminating
				delete(m.sess, t.VMID)
				m.mu.Unlock()
				m.finishTermination(sl, s)
				continue
			default:
				m.mu.Unlock()
				return nil
			}
		}
		if len(m.slots) >= m.cfg.MaxSessions {
			m.mu.Unlock()
			return errors.New("terminal: too many active terminals")
		}
		sctx, scancel := context.WithCancel(context.Background())
		sl := &slot{vmID: t.VMID, token: t.Token, state: slotStarting, cancel: scancel, ready: make(chan struct{}), done: make(chan struct{})}
		m.slots[t.VMID] = sl
		m.pending[t.VMID] = sl
		m.mu.Unlock()
		return m.launch(ctx, sctx, sl, t)
	}
}

// finishTermination is run by the single owner that moved sl to terminating. It
// kills everything, removes the files and only then releases the reservation.
// s may be nil (a start that never produced a backend).
func (m *Manager) finishTermination(sl *slot, s *session) {
	if s != nil {
		s.terminate(m)
	}
	if sl.releaseKey != nil {
		sl.releaseKey()
	}
	os.Remove(m.KnownHostsPath(sl.vmID))
	os.Remove(m.sockPath(sl.vmID))
	m.mu.Lock()
	if m.slots[sl.vmID] == sl {
		delete(m.slots, sl.vmID)
	}
	m.mu.Unlock()
	close(sl.done)
}

// launch runs the slow part of Start without holding m.mu. A Revoke or Close
// that arrives meanwhile cancels sctx; the half-started backend is then killed
// and Start fails, so a revoked terminal can never come up afterwards.
func (m *Manager) launch(ctx, sctx context.Context, sl *slot, t TerminalTarget) (err error) {
	var s *session
	defer func() {
		m.mu.Lock()
		delete(m.pending, t.VMID)
		keep := err == nil && s != nil && !m.closed && sctx.Err() == nil && (t.authorize == nil || t.authorize())
		if keep {
			sl.state = slotActive
			m.sess[t.VMID] = s
			m.mu.Unlock()
			sl.cancel()
			close(sl.ready)
			return
		}
		sl.state = slotTerminating
		m.mu.Unlock()
		sl.cancel()
		close(sl.ready)
		if err == nil {
			err = errors.New("terminal: revoked during startup")
		}
		// The slot stays reserved (and Revoke/Close keep waiting on it) until the
		// half-started backend and its files are completely gone.
		m.finishTermination(sl, s)
	}()
	kh := m.KnownHostsPath(t.VMID)
	cfg := m.cfg
	// Only durable per-VM image authentication may select the legacy key while
	// access is enabled. In particular, ErrNotFound is never a policy decision.
	if t.AuthMode == vmlib.AuthProvisionPublicKey && cfg.Credentials == nil {
		return errors.New("terminal: dedicated VM credential unavailable")
	}
	if cfg.Credentials != nil && t.AuthMode != vmlib.AuthImagePreinstalled {
		path, release, e := cfg.Credentials.TerminalKeyFile(t.VMID, cfg.KeyRuntimeDir)
		if e != nil {
			return errors.New("terminal: dedicated VM credential unavailable")
		}
		if release == nil {
			return errors.New("terminal: invalid credential lease")
		}
		var once sync.Once
		sl.releaseKey = func() { once.Do(release) }
		cfg.SSHKeyFile = path
	}
	sshArgs, err := BuildSSHArgs(cfg, t, kh)
	if err != nil {
		return err
	}
	sock := m.sockPath(t.VMID)
	if len(sock) > 100 {
		return errors.New("terminal: state dir path too long for a unix socket")
	}
	if err := os.MkdirAll(filepath.Dir(kh), 0o700); err != nil {
		return fmt.Errorf("terminal: known_hosts dir: %w", err)
	}
	f, err := os.OpenFile(kh, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("terminal: known_hosts: %w", err)
	}
	f.Close()
	os.Remove(sock)

	args := append([]string{"--writable", "--check-origin", "--max-clients", fmt.Sprint(m.cfg.MaxClients), "--interface", sock}, sshArgs...)
	cmd := exec.Command(m.cfg.TTYDPath, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		os.Remove(kh)
		return errors.New("terminal: backend failed to start")
	}
	rctx, rcancel := context.WithCancel(context.Background())
	s = &session{vmID: t.VMID, token: t.Token, sock: sock, cmd: cmd, exited: make(chan struct{}), max: m.cfg.MaxClients,
		ctx: rctx, cancel: rcancel, conns: map[net.Conn]struct{}{}}
	s.authorize = t.authorize
	go func() {
		cmd.Wait()
		if sl.releaseKey != nil {
			sl.releaseKey()
		}
		close(s.exited)
	}()

	wctx, wcancel := context.WithCancel(ctx)
	stop := context.AfterFunc(sctx, wcancel) // Revoke/Close abort the wait
	defer func() { stop(); wcancel() }()
	if err := s.waitReady(wctx, m.cfg.StartTimeout); err != nil {
		return err
	}
	s.proxy = m.newProxy(s)
	return nil
}

func (s *session) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		c, err := net.DialTimeout("unix", s.sock, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		select {
		case <-s.exited:
			return errors.New("terminal: backend exited during startup (unsupported ttyd options?)")
		case <-ctx.Done():
			return errors.New("terminal: startup canceled")
		case <-deadline.C:
			return errors.New("terminal: backend did not come up in time")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Revoke removes one VM's link, closes its open connections, kills its ttyd and
// the ssh processes beneath it and deletes its known_hosts. It returns after
// that backend is gone (a start in flight is cancelled and awaited). Other VMs
// are untouched.
func (m *Manager) Revoke(vmID string) {
	if vmlib.ValidateID("vm", vmID) != nil {
		return
	}
	m.mu.Lock()
	sl := m.slots[vmID]
	if sl == nil {
		// Nothing running or being cleaned up for this VM: remove leftovers while
		// holding the lock so a concurrent Start cannot interleave with the unlink.
		os.Remove(m.KnownHostsPath(vmID))
		os.Remove(m.sockPath(vmID))
		m.mu.Unlock()
		return
	}
	var own *session
	switch sl.state {
	case slotActive:
		own = m.sess[vmID]
		delete(m.sess, vmID)
		sl.state = slotTerminating
	case slotStarting:
		sl.cancel() // the Start goroutine owns the cleanup
	}
	m.mu.Unlock()
	if own != nil {
		m.finishTermination(sl, own)
		return
	}
	<-sl.done // another goroutine owns the cleanup; return only after it is complete
}

// Close revokes every terminal; the manager cannot start terminals afterwards.
// It does not touch any VM.
func (m *Manager) Close() {
	type owned struct {
		sl *slot
		s  *session
	}
	m.mu.Lock()
	m.closed = true
	var mine []owned
	var all []*slot // every slot, whoever owns its cleanup
	for id, sl := range m.slots {
		all = append(all, sl)
		switch sl.state {
		case slotActive:
			mine = append(mine, owned{sl, m.sess[id]})
			delete(m.sess, id)
			sl.state = slotTerminating
		case slotStarting:
			sl.cancel()
		}
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, o := range mine {
		wg.Add(1)
		go func() { defer wg.Done(); m.finishTermination(o.sl, o.s) }()
	}
	wg.Wait()
	for _, sl := range all { // including cleanups owned by other goroutines
		<-sl.done
	}
}

func (s *session) terminate(m *Manager) {
	s.mu.Lock()
	s.revoked = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	s.cancel()
	for _, c := range conns {
		c.Close()
	}
	if s.cmd.Process != nil {
		pid := s.cmd.Process.Pid
		kids := descendants(pid)
		signalAll(pid, kids, syscall.SIGTERM)
		select {
		case <-s.exited:
		case <-time.After(2 * time.Second):
		}
		kids = append(kids, descendants(pid)...)
		signalAll(pid, kids, syscall.SIGKILL)
		<-s.exited
	}
	os.Remove(s.sock)
	os.Remove(m.KnownHostsPath(s.vmID))
}

// signalAll signals ttyd's process group and every descendant (each ssh runs in
// its own session, so the group kill alone would miss it).
func signalAll(ttyd int, kids []int, sig syscall.Signal) {
	_ = syscall.Kill(-ttyd, sig)
	for _, p := range kids {
		_ = syscall.Kill(-p, sig)
		_ = syscall.Kill(p, sig)
	}
	_ = syscall.Kill(ttyd, sig)
}

// descendants lists all transitive children of pid using /proc.
func descendants(pid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for _, e := range ents {
		var p int
		if _, err := fmt.Sscanf(e.Name(), "%d", &p); err != nil || fmt.Sprint(p) != e.Name() {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		i := strings.LastIndexByte(string(b), ')')
		if i < 0 {
			continue
		}
		var state string
		var ppid int
		if _, err := fmt.Sscanf(string(b[i+1:]), " %s %d", &state, &ppid); err != nil {
			continue
		}
		children[ppid] = append(children[ppid], p)
	}
	var out []int
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// ---- gateway ----

func (m *Manager) securityHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	ws := "wss://"
	if m.origin.Scheme == "http" {
		ws = "ws://"
	}
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
		"connect-src 'self' "+ws+m.origin.Host+"; img-src 'self' data:; font-src 'self' data:; "+
		"frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Del("Set-Cookie")
}

func (m *Manager) newProxy(s *session) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "ttyd"
			pr.Out.URL.RawQuery = ""
			pr.Out.Host = m.origin.Host // ttyd --check-origin compares Origin with Host
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Referer")
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", s.sock)
			},
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		ModifyResponse: func(res *http.Response) error { m.securityHeaders(res.Header); return nil },
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			m.securityHeaders(w.Header())
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
}

func (m *Manager) reject(w http.ResponseWriter, code int) {
	m.securityHeaders(w.Header())
	http.Error(w, http.StatusText(code), code)
}

var allowedTails = map[string]string{"": "/", "/": "/", "/token": "/token", "/ws": "/ws", "/auth_token.js": "/auth_token.js"}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// Handler is the public HTTP(S) handler. Every request, including resources
// and WebSockets, needs the secret path token, an exact Host and (for
// WebSockets) an exact Origin. Forwarded headers are never consulted.
func (m *Manager) Handler() http.Handler { return http.HandlerFunc(m.serve) }

func (m *Manager) serve(w http.ResponseWriter, r *http.Request) {
	if r.Host != m.origin.Host {
		m.reject(w, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		m.reject(w, http.StatusMethodNotAllowed)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/t/")
	if !ok {
		m.reject(w, http.StatusNotFound)
		return
	}
	tok, tail, _ := strings.Cut(rest, "/")
	if tail != "" || strings.HasSuffix(rest, "/") {
		tail = "/" + tail
	}
	s := m.lookup(tok)
	if s == nil {
		m.reject(w, http.StatusNotFound)
		return
	}
	target, ok := allowedTails[tail]
	if !ok {
		m.reject(w, http.StatusNotFound)
		return
	}
	if r.URL.RawQuery != "" {
		m.reject(w, http.StatusBadRequest) // no URL arguments may ever reach ttyd
		return
	}
	origin := r.Header.Get("Origin")
	want := m.origin.Scheme + "://" + m.origin.Host
	if (origin != "" && origin != want) || (target == "/ws" && origin == "") {
		m.reject(w, http.StatusForbidden)
		return
	}
	if target == "/ws" && !isUpgrade(r) {
		m.reject(w, http.StatusBadRequest)
		return
	}
	tw := &trackedWriter{ResponseWriter: w, s: s}
	if target == "/ws" {
		if !s.acquire() {
			m.reject(w, http.StatusServiceUnavailable)
			return
		}
		tw.slot = true
	}
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	out := r.Clone(ctx)
	out.URL.Path, out.URL.RawPath = target, ""
	s.proxy.ServeHTTP(tw, out)
	if tw.slot && !tw.hijacked {
		s.release()
	}
}

// lookup finds the session for a capability token. It compares against every
// session (no early exit) and holds the lock only for the lookup itself.
func (m *Manager) lookup(tok string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found *session
	for _, s := range m.sess {
		if subtle.ConstantTimeCompare([]byte(s.token), []byte(tok)) == 1 && s.authorized() {
			found = s
		}
	}
	return found
}

func (s *session) authorized() bool { return s.authorize == nil || s.authorize() }

func (s *session) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked || !s.authorized() || s.clients >= s.max {
		return false
	}
	s.clients++
	return true
}

func (s *session) release() {
	s.mu.Lock()
	s.clients--
	s.mu.Unlock()
}

// trackedWriter records hijacked (WebSocket) connections so revocation can
// close them; http.Server.Shutdown does not.
type trackedWriter struct {
	http.ResponseWriter
	s        *session
	slot     bool
	hijacked bool
}

func (w *trackedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.s.mu.Lock()
	if w.s.revoked || !w.s.authorized() {
		w.s.mu.Unlock()
		return nil, nil, errors.New("terminal revoked")
	}
	conn, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		w.s.mu.Unlock()
		return nil, nil, err
	}
	w.hijacked = true
	tc := &trackedConn{Conn: conn, s: w.s, slot: w.slot}
	w.s.conns[tc] = struct{}{}
	w.s.mu.Unlock()
	return tc, brw, nil
}

type trackedConn struct {
	net.Conn
	s    *session
	slot bool
	once sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.s.mu.Lock()
		delete(c.s.conns, c)
		if c.slot {
			c.s.clients--
		}
		c.s.mu.Unlock()
	})
	return err
}
