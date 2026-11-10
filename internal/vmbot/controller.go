package vmbot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Sentinel errors. Their text is safe to show to the user: it never contains
// RPC internals, paths, tokens or links.
var (
	ErrCapacity     = errors.New("application VM limit reached")
	ErrBackendFull  = errors.New("backend has no free capacity")
	ErrNameTaken    = errors.New("VM name already in use")
	ErrBadName      = errors.New("invalid VM name")
	ErrNeedSelector = errors.New("several VMs: a name or id is required")
	ErrNoVM         = errors.New("no such VM owned by this bot")
	ErrUncertain    = errors.New("remote state is uncertain")
	ErrBusy         = errors.New("another operation is in progress")
	ErrStale        = errors.New("stale VM generation")
	ErrNotRunning   = errors.New("VM is not running or its terminal is revoked")
	ErrFailed       = errors.New("operation failed")
)

// DefaultMaxVMs is the application ceiling when none is configured.
const DefaultMaxVMs = 2

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
	AllowedUserIDs   []int64
	MaxVMsPerUser    int
	ImageID, Profile string
	PublicOrigin     string
	MaxVMs           int           // application ceiling on owned VMs; default 2
	OpTimeout        time.Duration // bounds one Create/Delete RPC
	QueryTimeout     time.Duration // bounds Get/List/SSHInfo and how long status waits for a busy VM
	Logger           *log.Logger
}

// Ref selects one VM: by label or ID (Sel), by generation (Gen, used by
// buttons), or, with both empty, "the only VM". A generation names one VM
// forever, so a stale Ref can never retarget a replacement.
type Ref struct {
	Sel string
	Gen uint64
}

// Status is the user-facing view of one VM.
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
// vmlib.Lifecycle. It never touches VMs it did not create. All remote and
// lifecycle work runs as DBOS workflows (orch.go, ADR 0004): the queue
// partition per VM is the only per-VM serializer.
type Controller struct {
	lc   vmlib.Lifecycle
	st   *Store
	term Terminals
	cfg  ControllerConfig

	eng      *engine
	engMu    sync.RWMutex
	startMu  sync.Mutex
	intentMu sync.Mutex // short gate: delete-intent decisions only
}

func NewController(lc vmlib.Lifecycle, st *Store, term Terminals, cfg ControllerConfig) *Controller {
	if cfg.OpTimeout <= 0 {
		cfg.OpTimeout = 5 * time.Minute
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 20 * time.Second
	}
	if cfg.MaxVMs <= 0 {
		cfg.MaxVMs = DefaultMaxVMs
	}
	if cfg.MaxVMsPerUser <= 0 {
		cfg.MaxVMsPerUser = 1
	}
	cfg.AllowedUserIDs = append([]int64(nil), cfg.AllowedUserIDs...)
	cfg.PublicOrigin = strings.TrimRight(cfg.PublicOrigin, "/")
	return &Controller{lc: lc, st: st, term: term, cfg: cfg}
}

type userKey struct{}

// WithUser attaches the authenticated Telegram sender, never a selector or payload identity.
// Only trusted transport adapters should establish this identity. Missing/zero is denied.
func WithUser(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, userKey{}, userID)
}
func caller(ctx context.Context) int64 { id, _ := ctx.Value(userKey{}).(int64); return id }
func (c *Controller) allowedUser(id int64) bool {
	if id <= 0 {
		return false
	}
	for _, allowed := range c.cfg.AllowedUserIDs {
		if id == allowed {
			return true
		}
	}
	return false
}
func (c *Controller) ownedState(id int64) State {
	s := c.st.Snapshot()
	out := s.VMs[:0]
	for _, r := range s.VMs {
		if r.OwnerUserID == id {
			out = append(out, r)
		}
	}
	s.VMs = out
	return s
}

func (c *Controller) logf(format string, a ...any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Printf(format, a...)
	}
}

// MaxVMs is the effective application ceiling.
func (c *Controller) MaxVMs() int { return c.cfg.MaxVMs }

func (c *Controller) opWait() time.Duration { return c.cfg.OpTimeout + c.cfg.QueryTimeout }

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
	s := Status{Present: true, Generation: rec.Generation, VMID: rec.ID, Name: rec.Label, Phase: rec.Phase}
	if vm != nil {
		s.State, s.Retained, s.Reason = string(vm.State), vm.Retained, vm.RetainedReason
	}
	return s
}

func (c *Controller) query(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), c.cfg.QueryTimeout)
}

// Resolve maps a Ref to a durable record without any remote call.
func (c *Controller) Resolve(userID int64, ref Ref) (VMRecord, error) {
	if !c.allowedUser(userID) {
		return VMRecord{}, ErrNoVM
	}
	st := c.ownedState(userID)
	switch {
	case ref.Gen != 0:
		r, ok := st.ByGeneration(ref.Gen)
		if !ok || (ref.Sel != "" && r.Label != ref.Sel && r.ID != ref.Sel) {
			return VMRecord{}, ErrStale
		}
		return r, nil
	case ref.Sel != "":
		r, ok := st.BySelector(ref.Sel)
		if !ok {
			return VMRecord{}, ErrNoVM
		}
		return r, nil
	}
	switch len(st.VMs) {
	case 0:
		return VMRecord{}, ErrNoVM
	case 1:
		return st.VMs[0], nil
	}
	return VMRecord{}, ErrNeedSelector
}

func (c *Controller) setPhase(id, from, to string) error {
	return c.st.Update(func(s *State) {
		s.update(id, func(r *VMRecord) {
			if r.Phase == from {
				r.Phase = to
			}
		})
	})
}

func (c *Controller) drop(id string) error {
	return c.st.Update(func(s *State) { s.remove(id) })
}

// reconcileLocked brings one VM's durable record in line with the remote VM. It
// runs inside a workflow step, so it is the only operation on that VM at the
// time. ErrUncertain means the remote could not be confirmed and the occupancy
// is kept. A definitively missing VM releases its record.
func (c *Controller) reconcileLocked(ctx context.Context, rec VMRecord) (Status, error) {
	qctx, cancel := c.query(ctx)
	defer cancel()
	vm, err := c.lc.Get(qctx, rec.ID)
	switch {
	case errors.Is(err, vmlib.ErrNotFound):
		// NotFound alone never releases the record: a Create that is still running
		// (or a delayed one) may yet bring the VM into existence. Only a
		// successful Delete, which waits for the VM's operations to settle and
		// then blocks late creates, proves it is gone.
		if !c.confirmGoneLocked(ctx, rec) {
			s := statusOf(&rec, nil)
			s.Uncertain = true
			return s, fmt.Errorf("%w: VM not visible but its absence is not proven", ErrUncertain)
		}
		return Status{}, nil
	case err != nil:
		c.logf("reconcile %s: get failed: %v", rec.ID, err)
		s := statusOf(&rec, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w", ErrUncertain)
	}
	if rec.Phase == PhaseDeleting {
		c.term.Revoke(rec.ID) // idempotent; the terminal must never come back
	}
	if rec.Phase == PhaseCreating {
		if uerr := c.setPhase(rec.ID, PhaseCreating, PhaseActive); uerr != nil {
			return statusOf(&rec, &vm), fmt.Errorf("%w: cannot persist state", ErrUncertain)
		}
		rec.Phase = PhaseActive
	}
	if rec.Phase == PhaseActive && vm.State == vmlib.StateRunning {
		if terr := c.ensureTerminalLocked(qctx, rec); terr != nil {
			c.logf("terminal start failed for %s: %v", rec.ID, terr)
		}
	}
	return statusOf(&rec, &vm), nil
}

// confirmGoneLocked uses vmlib's Delete as the settle-and-block barrier for a VM
// that Get reports as NotFound. On proof it revokes the terminal and clears the
// durable record; otherwise the occupancy is kept.
func (c *Controller) confirmGoneLocked(ctx context.Context, rec VMRecord) bool {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.OpTimeout)
	res, err := c.lc.Delete(dctx, rec.ID)
	cancel()
	if err != nil || !res.Deleted {
		if err != nil {
			c.logf("absence barrier %s: %v", rec.ID, err)
		}
		return false
	}
	c.term.Revoke(rec.ID)
	if uerr := c.drop(rec.ID); uerr != nil {
		c.logf("absence barrier %s: cannot persist state: %v", rec.ID, uerr)
		return false
	}
	return true
}

// ensureTerminalLocked (re)starts one VM's terminal backend from DURABLE state:
// it grants only for phase active and re-checks after the backend is up. A
// delete intent persisted in between (the command persists it before it
// revokes) is therefore always noticed by one of the two sides.
func (c *Controller) ensureTerminalLocked(ctx context.Context, rec VMRecord) error {
	cur, ok := c.st.Snapshot().ByID(rec.ID)
	if !ok || cur.Phase != PhaseActive || !c.allowedUser(cur.OwnerUserID) {
		c.term.Revoke(rec.ID)
		return ErrNotRunning
	}
	info, err := c.lc.SSHInfo(ctx, cur.ID)
	if err != nil {
		return fmt.Errorf("ssh info: %w", err)
	}
	authorized := func() bool {
		r, ok := c.st.Snapshot().ByID(cur.ID)
		return ok && c.allowedUser(r.OwnerUserID) && r.OwnerUserID == cur.OwnerUserID && r.Phase == PhaseActive && r.Generation == cur.Generation && r.Token == cur.Token
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

// Recover is called at startup, after Start has launched the orchestration layer
// (which already resumed this executor's pending workflows). It re-submits the
// durable intent of every record and rebuilds the terminal projections:
//
//	creating -> its create workflow (idempotent by ID), or a guarded reconcile
//	            when that workflow already concluded
//	deleting -> the delete workflow of the current attempt; terminals stay revoked
//	active   -> reconcile, which re-attaches the terminal of a running VM
//
// Records are independent: one failure never hides or deletes another.
func (c *Controller) Recover(ctx context.Context) error {
	recs := c.st.Snapshot().VMs
	errs := make([]error, len(recs))
	var wg sync.WaitGroup
	for i, r := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.recoverOne(r)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (c *Controller) recoverOne(rec VMRecord) error {
	switch rec.Phase {
	case PhaseDeleting:
		h, _, err := c.submitDelete(rec)
		if err != nil {
			return err
		}
		r, err := c.wait(h, c.opWait())
		if err != nil {
			return err
		}
		if r.Code != codeOK {
			return r.err()
		}
		return nil
	default:
		s, err := c.reconcileVM(rec, c.opWait())
		if err == nil && s.Busy {
			return ErrBusy
		}
		return err
	}
}

// List reports every owned VM, reconciling each first. Uncertain VMs are listed
// with Uncertain=true and busy ones from durable data; none is dropped.
func (c *Controller) List(ctx context.Context) ([]Status, error) {
	if !c.allowedUser(caller(ctx)) {
		return nil, ErrNoVM
	}
	recs := c.ownedState(caller(ctx)).VMs
	out := make([]Status, len(recs))
	var wg sync.WaitGroup
	for i, r := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := c.reconcileVM(r, c.cfg.QueryTimeout)
			if err != nil && !errors.Is(err, ErrUncertain) {
				s = statusOf(&r, nil)
				s.Busy = true
			}
			out[i] = s
		}()
	}
	wg.Wait()
	res := make([]Status, 0, len(out))
	for _, s := range out {
		if s.Present {
			res = append(res, s)
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Generation < res[j].Generation })
	return res, nil
}

// Status reports one VM, reconciling first.
func (c *Controller) Status(ctx context.Context, ref Ref) (Status, error) {
	rec, err := c.Resolve(caller(ctx), ref)
	if err != nil {
		return Status{}, err
	}
	return c.reconcileVM(rec, c.cfg.QueryTimeout)
}

func pickLabel(s *State, owner int64, want string) (string, error) {
	taken := func(l string) bool {
		for _, r := range s.VMs {
			if r.OwnerUserID == owner && r.Label == l {
				return true
			}
		}
		return false
	}
	if want != "" {
		if taken(want) {
			return "", ErrNameTaken
		}
		return want, nil
	}
	l := fmt.Sprintf("vm-%d", s.Generation)
	for k := 2; taken(l); k++ {
		l = fmt.Sprintf("vm-%d-%d", s.Generation, k)
	}
	return l, nil
}

// Create makes another VM. name may be empty (a short label is generated).
// Admission (capacity, unique name, generation) is one atomic state update that
// also makes the create intent durable; the create workflow then runs on the
// VM's own queue partition, so a slow Create never blocks another VM.
func (c *Controller) Create(ctx context.Context, name string) (Status, error) {
	owner := caller(ctx)
	if !c.allowedUser(owner) {
		return Status{}, ErrNoVM
	}
	if name != "" && ValidateLabel(name) != nil {
		return Status{}, ErrBadName
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
	if err := c.st.UpdateErr(func(s *State) error {
		if len(s.VMs) >= c.cfg.MaxVMs {
			return ErrCapacity
		}
		n := 0
		for _, r := range s.VMs {
			if r.OwnerUserID == owner {
				n++
			}
		}
		if n >= c.cfg.MaxVMsPerUser {
			return ErrCapacity
		}
		s.Generation++
		label, lerr := pickLabel(s, owner, name)
		if lerr != nil {
			return lerr
		}
		rec = VMRecord{OwnerUserID: owner, ID: id, Token: tok, Phase: PhaseCreating, Generation: s.Generation, Label: label}
		s.VMs = append(s.VMs, rec)
		return nil
	}); err != nil {
		if errors.Is(err, ErrCapacity) || errors.Is(err, ErrNameTaken) {
			return Status{}, err
		}
		return Status{}, fmt.Errorf("%w: cannot persist state", ErrFailed)
	}
	h, err := c.ensureCreate(rec)
	if err != nil { // the intent is durable; Recover/Status will submit it
		s := statusOf(&rec, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w: creation not yet submitted", ErrUncertain)
	}
	r, err := c.wait(h, c.opWait())
	if err != nil {
		s := statusOf(&rec, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w: creation still in progress", ErrUncertain)
	}
	if r.Code != codeOK {
		return r.Status, r.err()
	}
	return r.Status, nil
}

// Terminal returns the secret link for one VM, (re)starting its backend.
func (c *Controller) Terminal(ctx context.Context, ref Ref) (string, error) {
	rec, err := c.Resolve(caller(ctx), ref)
	if err != nil {
		return "", err
	}
	if rec.Phase == PhaseCreating { // settle the create first (guarded)
		if _, err := c.reconcileVM(rec, c.opWait()); err != nil {
			return "", err
		}
	}
	id, err := c.newRunID(opTerminal, rec.ID)
	if err != nil {
		return "", fmt.Errorf("%w: random source", ErrFailed)
	}
	h, err := c.submit(OpInput{Kind: opTerminal, VMID: rec.ID, OwnerUserID: rec.OwnerUserID}, id)
	if err != nil {
		return "", err
	}
	r, err := c.wait(h, c.opWait())
	if err != nil {
		return "", err
	}
	if r.Code != codeOK {
		return "", r.err()
	}
	// The link is built from durable state now, never from the workflow result.
	cur, ok := c.st.Snapshot().ByID(rec.ID)
	if !ok || cur.Phase != PhaseActive || cur.OwnerUserID != caller(ctx) || !c.allowedUser(cur.OwnerUserID) {
		return "", ErrNotRunning
	}
	return c.cfg.PublicOrigin + "/t/" + cur.Token + "/", nil
}

// Delete removes one VM; it never acts without an unambiguous Ref. Ref.Gen
// binds the call to one generation so a stale button cannot hit a replacement.
func (c *Controller) Delete(ctx context.Context, ref Ref) (Status, error) {
	rec, err := c.Resolve(caller(ctx), ref)
	if err != nil {
		return Status{}, err
	}
	h, cur, err := c.submitDelete(rec)
	if err != nil {
		r := cur
		r.Phase = PhaseDeleting
		if cur.ID == "" {
			return Status{}, err
		}
		return statusOf(&r, nil), err
	}
	r, err := c.wait(h, c.opWait())
	if err != nil {
		cur.Phase = PhaseDeleting
		s := statusOf(&cur, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w: delete still in progress", ErrFailed)
	}
	if r.Code != codeOK {
		return r.Status, r.err()
	}
	return r.Status, nil
}
