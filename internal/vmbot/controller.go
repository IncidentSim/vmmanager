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

type ControllerConfig struct {
	Profile      string
	PublicOrigin string
	OpTimeout    time.Duration // bounds one Create/Delete RPC
	QueryTimeout time.Duration // bounds Get/List/SSHInfo and how long status waits for a busy VM
	Logger       *log.Logger
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
	// authorizer is an optional extra allow decision (durable username bindings).
	authorizer func(int64) bool
	lc         vmlib.Lifecycle
	st         *Store
	term       Terminals
	cfg        ControllerConfig

	// testBeforeProject is a test seam called at the start of the projection step
	// (after the child completed, before the outcome is written). Nil in production.
	testBeforeProject func(OwnerCmd)

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
	cfg.PublicOrigin = strings.TrimRight(cfg.PublicOrigin, "/")
	return &Controller{lc: lc, st: st, term: term, cfg: cfg}
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
	if err := c.term.Start(ctx, TerminalTarget{VMID: cur.ID, Token: cur.Token, Host: info.Host, User: info.User, ProxyJump: info.ProxyJump(), authorize: authorized}); err != nil {
		return err
	}
	if after, ok := c.st.Snapshot().ByID(cur.ID); !ok || after.Phase != PhaseActive {
		c.term.Revoke(cur.ID)
		return ErrNotRunning
	}
	return nil
}
