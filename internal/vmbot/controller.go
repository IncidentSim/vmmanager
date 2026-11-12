package vmbot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Sentinel errors. Their text is safe to show to the user: it never contains
// RPC internals, paths, tokens or links.
var (
	ErrCapacity   = errors.New("application VM limit reached")
	ErrNoVM       = errors.New("no such VM owned by this bot")
	ErrUncertain  = errors.New("remote state is uncertain")
	ErrNotRunning = errors.New("VM is not running or its terminal is revoked")
	ErrFailed     = errors.New("operation failed")
)

// TerminalTarget is everything the terminal backend may know about a guest.
type TerminalTarget struct {
	VMID, Token, Host, User, ProxyJump string
	// AuthMode comes from the backend's per-VM SSH policy, never the caller.
	AuthMode string
	// authorize is a live domain-state fence. A start can outlive its command's
	// active-state check; publication and new HTTP/WS access must revalidate.
	authorize func() bool
}

// Terminals is the terminal manager seam (real: Manager; tests: fake).
type Terminals interface {
	Start(ctx context.Context, t TerminalTarget) error
	Revoke(vmID string)
	Close()
}

// CredentialDeleter removes per-VM credentials idempotently after proven deletion.
// It deliberately exposes no secret-bearing reads to the durable controller.
type CredentialDeleter interface {
	Delete(vmID string) error
}

type ControllerConfig struct {
	// Credentials is shared with backend access and terminals. Nil disables cleanup
	// for legacy installations without per-VM credentials.
	Credentials  CredentialDeleter
	Profile      string
	PublicOrigin string
	OpTimeout    time.Duration // bounds one Create/Delete RPC
	QueryTimeout time.Duration // bounds Get/List/SSHInfo and how long status waits for a busy VM
	// DefaultTTL/MaxTTL bound a created VM's lifetime (defaults 3600s / 86400s).
	// Field-aware validation of these settings belongs to the config layer.
	DefaultTTL, MaxTTL time.Duration
	// IdleProbe enables SSH-idle expiry (docs/adr/0008-ssh-idle-expiry.md): a VM
	// with no established SSH connection for more than IdleTimeout is deleted
	// through its owner. Nil IdleProbe or a zero IdleTimeout disables it (no
	// trusted observation exists, so nothing is ever deleted for idleness).
	// IdlePoll defaults to 30s.
	IdleProbe   ConnectionCounter
	IdleTimeout time.Duration
	IdlePoll    time.Duration
	Logger      *log.Logger
}

// Status is the VM view carried in OpResult, the retired workflow's stored
// output shape (see orch.go).
type Status struct {
	Present    bool
	Generation uint64
	VMID       string
	Name       string
	State      string // vmlib state when known
	Phase      string
	Retained   bool
	Reason     string
	Uncertain  bool // remote state could not be confirmed
	Busy       bool // another operation on this VM is running; durable data only
}

// Controller owns the VMs recorded in its durable state on top of
// vmlib.Lifecycle. It never touches VMs it did not create. Control stores run
// lifecycle work through the per-VM owner workflows (owner.go, control.go).
type Controller struct {
	control ControlPolicy
	admins  []int64 // immutable after configuration, separate from ordinary access
	adminMu sync.Mutex
	// authorizer is an optional extra allow decision (durable username bindings).
	authorizer func(int64) bool
	lc         vmlib.Lifecycle
	st         *Store
	term       Terminals
	cfg        ControllerConfig

	// testBeforeProject is a test seam called at the start of the projection step
	// (after the child completed, before the outcome is written). Nil in production.
	testBeforeProject func(OwnerCmd)

	// ownerFn is the registered owner workflow body (vmOwner). Tests substitute a
	// frozen baseline body to produce genuine old histories. Nil means vmOwner.
	ownerFn func(dbos.Context, OwnerIn) (OwnerOut, error)
	// clock is a test seam for admission/expiry decisions; nil means time.Now.
	clock func() time.Time

	// idle is the in-memory SSH-idle window per VM; idleStop/idleDone belong to
	// the monitor goroutine started by Start and ended by Close.
	idle     idleTracker
	idleStop context.CancelFunc
	idleDone chan struct{}

	// rebuild holds the VM IDs whose terminal is rebuilt once after Start.
	rebuild sync.Map

	eng     *engine
	engMu   sync.RWMutex
	startMu sync.Mutex
}

func NewController(lc vmlib.Lifecycle, st *Store, term Terminals, cfg ControllerConfig) *Controller {
	if cfg.OpTimeout <= 0 {
		cfg.OpTimeout = 5 * time.Minute
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 20 * time.Second
	}
	if cfg.MaxTTL <= 0 {
		cfg.MaxTTL = MaxVMTTL
	}
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = DefaultVMTTL
	}
	if cfg.DefaultTTL > cfg.MaxTTL {
		cfg.DefaultTTL = cfg.MaxTTL
	}
	cfg.PublicOrigin = strings.TrimRight(cfg.PublicOrigin, "/")
	return &Controller{lc: lc, st: st, term: term, cfg: cfg}
}

func (c *Controller) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}
	return time.Now()
}

func (c *Controller) ownerBody() func(dbos.Context, OwnerIn) (OwnerOut, error) {
	if c.ownerFn != nil {
		return c.ownerFn
	}
	return vmOwner
}

func (c *Controller) logf(format string, a ...any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Printf(format, a...)
	}
}

func randToken() (string, error) {
	b := make([]byte, 32) // 256 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func newVMID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "bot-" + hex.EncodeToString(b), nil
}

// ensureTerminalLocked (re)starts one VM's terminal backend from DURABLE state:
// it grants only for phase active and re-checks after the backend is up. A
// delete intent persisted in between (the command persists it before it
// revokes) is therefore always noticed by one of the two sides.
func (c *Controller) ensureTerminalLocked(ctx context.Context, rec VMRecord) error {
	cur, ok := c.st.Snapshot().ByID(rec.ID)
	if !ok || cur.Phase != PhaseActive {
		c.term.Revoke(rec.ID)
		return ErrNotRunning
	}
	info, err := c.lc.SSHInfo(ctx, cur.ID)
	if err != nil {
		return fmt.Errorf("ssh info: %w", err)
	}
	authorized := func() bool {
		r, ok := c.st.Snapshot().ByID(cur.ID)
		return ok && (!c.st.Snapshot().Control || (r.OwnerUserID == cur.OwnerUserID && c.ControlAllowed(r.OwnerUserID))) && r.Phase == PhaseActive && r.Generation == cur.Generation && r.Token == cur.Token
	}
	host, jump := info.Host, info.ProxyJump()
	if info.InternalTarget != nil {
		if info.InternalTarget.Port != 22 {
			return fmt.Errorf("terminal internal SSH target must use port 22")
		}
		host, jump = info.InternalTarget.Host, ""
	}
	if err := c.term.Start(ctx, TerminalTarget{VMID: cur.ID, Token: cur.Token, Host: host, User: info.User, ProxyJump: jump, AuthMode: info.AuthMode, authorize: authorized}); err != nil {
		return err
	}
	if after, ok := c.st.Snapshot().ByID(cur.ID); !ok || after.Phase != PhaseActive {
		c.term.Revoke(cur.ID)
		return ErrNotRunning
	}
	return nil
}
