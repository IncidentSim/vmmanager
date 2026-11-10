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
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Sentinel errors. Their text is safe to show to the user: it never contains
// RPC internals, paths, tokens or links.
var (
	ErrOccupied   = errors.New("slot occupied by an existing VM")
	ErrNoVM       = errors.New("no VM owned by this bot")
	ErrUncertain  = errors.New("remote state is uncertain")
	ErrBusy       = errors.New("another operation is in progress")
	ErrStale      = errors.New("stale VM generation")
	ErrNotRunning = errors.New("VM is not running or its terminal is revoked")
	ErrFailed     = errors.New("operation failed")
)

// TerminalTarget is everything the terminal backend may know about a guest.
type TerminalTarget struct {
	VMID, Token, Host, User, ProxyJump string
}

// Terminals is the terminal manager seam (real: Manager; tests: fake).
type Terminals interface {
	Start(ctx context.Context, t TerminalTarget) error
	Revoke(vmID string)
	Close()
}

type ControllerConfig struct {
	ImageID, Profile string
	PublicOrigin     string
	OpTimeout        time.Duration // bounds one Create/Delete RPC
	QueryTimeout     time.Duration // bounds Get/List/SSHInfo and the lock wait of Status
	Logger           *log.Logger
}

// Status is the user-facing view of the single slot.
type Status struct {
	Present    bool
	Generation uint64
	VMID       string
	State      string // vmlib state when known
	Phase      string
	Retained   bool
	Reason     string
	Uncertain  bool // remote state could not be confirmed
}

// Controller enforces exactly one occupied VM on top of vmlib.Lifecycle. It
// owns only the VM recorded in its durable state and never touches others.
type Controller struct {
	lc   vmlib.Lifecycle
	st   *Store
	term Terminals
	cfg  ControllerConfig
	op   chan struct{}
}

func NewController(lc vmlib.Lifecycle, st *Store, term Terminals, cfg ControllerConfig) *Controller {
	if cfg.OpTimeout <= 0 {
		cfg.OpTimeout = 5 * time.Minute
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 20 * time.Second
	}
	cfg.PublicOrigin = strings.TrimRight(cfg.PublicOrigin, "/")
	return &Controller{lc: lc, st: st, term: term, cfg: cfg, op: make(chan struct{}, 1)}
}

func (c *Controller) logf(format string, a ...any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Printf(format, a...)
	}
}

func (c *Controller) lock(ctx context.Context) error {
	select {
	case c.op <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ErrBusy
	}
}
func (c *Controller) unlock() { <-c.op }

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

func statusOf(rec *VMRecord, vm *vmlib.VM) Status {
	if rec == nil {
		return Status{}
	}
	s := Status{Present: true, Generation: rec.Generation, VMID: rec.ID, Phase: rec.Phase}
	if vm != nil {
		s.State, s.Retained, s.Reason = string(vm.State), vm.Retained, vm.RetainedReason
	}
	return s
}

func (c *Controller) query(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), c.cfg.QueryTimeout)
}

// reconcileLocked brings durable state in line with the remote VM. It returns
// the current status; ErrUncertain means the remote could not be confirmed and
// occupancy is kept. A definitively missing VM releases the slot.
func (c *Controller) reconcileLocked(ctx context.Context) (Status, error) {
	rec := c.st.Snapshot().VM
	if rec == nil {
		return Status{}, nil
	}
	qctx, cancel := c.query(ctx)
	defer cancel()
	vm, err := c.lc.Get(qctx, rec.ID)
	switch {
	case errors.Is(err, vmlib.ErrNotFound):
		// NotFound alone never releases the slot: a Create that is still running
		// (or a delayed one) may yet bring the VM into existence. Only a
		// successful Delete, which waits for the VM's operations to settle and
		// then blocks late creates, proves it is gone.
		if !c.confirmGoneLocked(ctx, rec) {
			s := statusOf(rec, nil)
			s.Uncertain = true
			return s, fmt.Errorf("%w: VM not visible but its absence is not proven", ErrUncertain)
		}
		return Status{}, nil
	case err != nil:
		c.logf("reconcile: get failed: %v", err)
		s := statusOf(rec, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w", ErrUncertain)
	}
	if rec.Phase == PhaseDeleting {
		c.term.Revoke(rec.ID) // idempotent; the terminal must never come back
	}
	if rec.Phase == PhaseCreating {
		if uerr := c.st.Update(func(s *State) {
			if s.VM != nil && s.VM.ID == rec.ID && s.VM.Phase == PhaseCreating {
				s.VM.Phase = PhaseActive
			}
		}); uerr != nil {
			return statusOf(rec, &vm), fmt.Errorf("%w: cannot persist state", ErrUncertain)
		}
		rec = c.st.Snapshot().VM
	}
	if rec.Phase == PhaseActive && vm.State == vmlib.StateRunning {
		if terr := c.ensureTerminalLocked(qctx, rec); terr != nil {
			c.logf("terminal start failed: %v", terr)
		}
	}
	return statusOf(rec, &vm), nil
}

// confirmGoneLocked uses vmlib's Delete as the settle-and-block barrier for a VM
// that Get reports as NotFound. On proof it revokes the terminal and clears the
// durable record; otherwise the occupancy is kept.
func (c *Controller) confirmGoneLocked(ctx context.Context, rec *VMRecord) bool {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.OpTimeout)
	res, err := c.lc.Delete(dctx, rec.ID)
	cancel()
	if err != nil || !res.Deleted {
		if err != nil {
			c.logf("absence barrier: %v", err)
		}
		return false
	}
	c.term.Revoke(rec.ID)
	if uerr := c.st.Update(func(s *State) {
		if s.VM != nil && s.VM.ID == rec.ID {
			s.VM = nil
		}
	}); uerr != nil {
		c.logf("absence barrier: cannot persist state: %v", uerr)
		return false
	}
	return true
}

func (c *Controller) ensureTerminalLocked(ctx context.Context, rec *VMRecord) error {
	info, err := c.lc.SSHInfo(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("ssh info: %w", err)
	}
	return c.term.Start(ctx, TerminalTarget{VMID: rec.ID, Token: rec.Token, Host: info.Host, User: info.User, ProxyJump: info.ProxyJump()})
}

// Recover is called at startup: it reconciles any recorded VM with the remote.
// A remote that is unreachable is reported (the app keeps running, occupancy
// is kept and the next operation reconciles again).
func (c *Controller) Recover(ctx context.Context) error {
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
	_, err := c.reconcileLocked(ctx)
	return err
}

// Status reports the slot, reconciling first.
func (c *Controller) Status(ctx context.Context) (Status, error) {
	lctx, cancel := context.WithTimeout(ctx, c.cfg.QueryTimeout)
	defer cancel()
	if err := c.lock(lctx); err != nil {
		return Status{}, err
	}
	defer c.unlock()
	return c.reconcileLocked(ctx)
}

// Create makes the one VM. Duplicate calls are serialized and refused.
func (c *Controller) Create(ctx context.Context) (Status, error) {
	if err := c.lock(ctx); err != nil {
		return Status{}, err
	}
	defer c.unlock()

	cur, err := c.reconcileLocked(ctx)
	if err != nil {
		return cur, err
	}
	if cur.Present {
		return cur, ErrOccupied
	}
	qctx, cancel := c.query(ctx)
	vms, err := c.lc.List(qctx)
	cancel()
	if err != nil {
		c.logf("create: list failed: %v", err)
		return Status{}, fmt.Errorf("%w: cannot list existing VMs", ErrUncertain)
	}
	for _, v := range vms {
		if v.State != vmlib.StateDeleted {
			return Status{}, ErrOccupied // foreign VM on the shared facade; never adopted
		}
	}

	id, err := newVMID()
	if err != nil {
		return Status{}, fmt.Errorf("%w: random source", ErrFailed)
	}
	tok, err := randToken()
	if err != nil {
		return Status{}, fmt.Errorf("%w: random source", ErrFailed)
	}
	var rec VMRecord
	if err := c.st.Update(func(s *State) {
		s.Generation++
		rec = VMRecord{ID: id, Token: tok, Phase: PhaseCreating, Generation: s.Generation}
		s.VM = &rec
	}); err != nil {
		return Status{}, fmt.Errorf("%w: cannot persist state", ErrFailed)
	}

	cctx, ccancel := context.WithTimeout(ctx, c.cfg.OpTimeout)
	vm, cerr := c.lc.Create(cctx, vmlib.CreateRequest{ID: id, ImageID: c.cfg.ImageID, Profile: c.cfg.Profile,
		Labels: map[string]string{"app": "vmbot"}})
	ccancel()
	if cerr != nil {
		c.logf("create: %v", cerr)
		return c.classifyCreateFailure(ctx, &rec)
	}
	if err := c.st.Update(func(s *State) {
		if s.VM != nil && s.VM.ID == id {
			s.VM.Phase = PhaseActive
		}
	}); err != nil {
		return statusOf(&rec, &vm), fmt.Errorf("%w: cannot persist state", ErrUncertain)
	}
	rec.Phase = PhaseActive
	if vm.State == vmlib.StateRunning || vm.State == "" {
		tctx, tcancel := c.query(ctx)
		if terr := c.ensureTerminalLocked(tctx, &rec); terr != nil {
			c.logf("terminal start failed: %v", terr)
		}
		tcancel()
	}
	return statusOf(&rec, &vm), nil
}

// classifyCreateFailure resolves an unknown Create outcome with Get.
func (c *Controller) classifyCreateFailure(ctx context.Context, rec *VMRecord) (Status, error) {
	qctx, cancel := c.query(ctx)
	defer cancel()
	vm, err := c.lc.Get(qctx, rec.ID)
	switch {
	case errors.Is(err, vmlib.ErrNotFound):
		if c.confirmGoneLocked(ctx, rec) {
			return Status{}, fmt.Errorf("%w: VM was not created", ErrFailed)
		}
		s := statusOf(rec, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w: creation outcome unknown", ErrUncertain)
	case err != nil:
		s := statusOf(rec, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w: creation outcome unknown", ErrUncertain)
	}
	_ = c.st.Update(func(s *State) {
		if s.VM != nil && s.VM.ID == rec.ID && s.VM.Phase == PhaseCreating {
			s.VM.Phase = PhaseActive
		}
	})
	r := *rec
	r.Phase = PhaseActive
	return statusOf(&r, &vm), fmt.Errorf("%w: VM exists but creation reported an error", ErrFailed)
}

// Terminal returns the secret link for the current VM, (re)starting its backend.
func (c *Controller) Terminal(ctx context.Context) (string, error) {
	if err := c.lock(ctx); err != nil {
		return "", err
	}
	defer c.unlock()
	st, err := c.reconcileLocked(ctx)
	if err != nil {
		return "", err
	}
	if !st.Present {
		return "", ErrNoVM
	}
	rec := c.st.Snapshot().VM
	if rec == nil || rec.Phase != PhaseActive || st.State != string(vmlib.StateRunning) {
		return "", ErrNotRunning
	}
	qctx, cancel := c.query(ctx)
	defer cancel()
	if err := c.ensureTerminalLocked(qctx, rec); err != nil {
		c.logf("terminal start failed: %v", err)
		return "", fmt.Errorf("%w: terminal backend unavailable", ErrFailed)
	}
	return c.cfg.PublicOrigin + "/t/" + rec.Token + "/", nil
}

// Delete removes the current VM. gen != 0 binds the call to one generation so
// a stale button cannot hit a replacement. The terminal is revoked first.
func (c *Controller) Delete(ctx context.Context, gen uint64) (Status, error) {
	if err := c.lock(ctx); err != nil {
		return Status{}, err
	}
	defer c.unlock()
	rec := c.st.Snapshot().VM
	if rec == nil {
		return Status{}, ErrNoVM
	}
	if gen != 0 && gen != rec.Generation {
		return statusOf(rec, nil), ErrStale
	}
	// The irreversible deleting phase is durable BEFORE anything else happens.
	// If it cannot be persisted nothing is revoked or deleted and the caller is
	// told so: a restart must never re-authorize a link we claimed to revoke.
	if rec.Phase != PhaseDeleting {
		if err := c.st.Update(func(s *State) {
			if s.VM != nil && s.VM.ID == rec.ID {
				s.VM.Phase = PhaseDeleting
			}
		}); err != nil {
			return statusOf(rec, nil), fmt.Errorf("%w: cannot persist state; nothing was changed", ErrFailed)
		}
	}
	c.term.Revoke(rec.ID)
	r := *rec
	r.Phase = PhaseDeleting

	dctx, cancel := context.WithTimeout(ctx, c.cfg.OpTimeout)
	res, derr := c.lc.Delete(dctx, rec.ID)
	cancel()
	if derr != nil || !res.Deleted {
		if derr != nil {
			c.logf("delete: %v", derr)
		}
		// Only a successful Delete releases the slot. A Get that says NotFound is
		// not proof (a Create may still be settling); the retry, or the next
		// reconcile, runs the same barrier.
		qctx, qcancel := c.query(ctx)
		vm, gerr := c.lc.Get(qctx, rec.ID)
		qcancel()
		s := statusOf(&r, nil)
		if gerr == nil {
			s = statusOf(&r, &vm)
		} else {
			s.Uncertain = true
		}
		if errors.Is(derr, vmlib.ErrRetained) || res.Retained {
			s.Retained = true
			return s, fmt.Errorf("%w: VM retained, exit not proven", ErrFailed)
		}
		return s, fmt.Errorf("%w: delete not confirmed", ErrFailed)
	}
	if err := c.st.Update(func(s *State) {
		if s.VM != nil && s.VM.ID == rec.ID {
			s.VM = nil
		}
	}); err != nil {
		return statusOf(&r, nil), fmt.Errorf("%w: cannot persist state", ErrUncertain)
	}
	return Status{}, nil
}
