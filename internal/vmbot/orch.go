package vmbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite" // pure-Go SQLite system database

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// This file is the single durable orchestration layer (ADR 0004). Every
// lifecycle command and every recovery action is a DBOS workflow on the queue
// "vmops", partitioned by VM ID with concurrency 1. Domain truth stays in
// state.json; the SQLite system database is an execution log only.

const (
	orchAppName    = "vmbot"
	orchExecutorID = "vmbot" // fixed: a second executor would strand or re-run work
	// orchAppVersion is a constant, not the binary hash, so pending workflows are
	// recovered after a rebuild. Bump it only together with a drain/migration plan.
	orchAppVersion = "vmbot-orch-1"
	orchQueue      = "vmops"
	orchWorkflow   = "vmbot-op"
	orchSubdir     = "orchestration"
	orchDBName     = "dbos.db"
)

// Operation kinds.
const (
	opCreate    = "create"
	opDelete    = "delete"
	opTerminal  = "terminal"
	opReconcile = "reconcile"
)

// OpInput is the workflow input: identifiers only, never a token or a link.
type OpInput struct {
	Kind string
	VMID string
	Seq  uint64
}

// Result codes carried in OpResult (an outcome is a value, not a workflow error:
// DBOS would otherwise cache the failure under the workflow ID).
const (
	codeOK          = ""
	codeFailed      = "failed"
	codeUncertain   = "uncertain"
	codeBackendFull = "backend_full"
	codeNotRunning  = "not_running"
	codeNoVM        = "no_vm"
	codeStale       = "stale"
)

// OpResult is the workflow output.
type OpResult struct {
	Code   string
	Msg    string
	Status Status
}

type ctlKey struct{}

type engine struct {
	ctx       dbos.Context
	queue     dbos.Queue
	ready     atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (c *Controller) engine() *engine {
	c.engMu.RLock()
	defer c.engMu.RUnlock()
	return c.eng
}

// OrchestrationDBPath is where the SQLite system database lives for a state dir.
func OrchestrationDBPath(stateDir string) string {
	return filepath.Join(stateDir, orchSubdir, orchDBName)
}

// Start opens the durable orchestration layer and launches it (which recovers
// this executor's pending workflows). The state-directory lifetime flock held by
// the Store must already be taken: it is what keeps a second instance away from
// the same database. Start fails closed (no in-memory fallback, no automatic
// deletion of a damaged database).
func (c *Controller) Start(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.engine() != nil {
		return errors.New("orchestration already started")
	}
	if err := c.st.claimEngine(c); err != nil {
		return err
	}
	claimed := true
	defer func() {
		if claimed {
			c.st.releaseEngine(c)
		}
	}()
	dir := filepath.Join(c.st.dir, orchSubdir)
	if !filepath.IsAbs(dir) {
		return errors.New("orchestration: state dir must be absolute")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("orchestration dir: %w", err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm()&0o077 != 0 {
		return errors.New("orchestration dir must be private (0700)")
	}
	dbPath := filepath.Join(dir, orchDBName)
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	dctx, err := dbos.NewContext(context.WithValue(context.WithoutCancel(ctx), ctlKey{}, c), dbos.Config{
		AppName: orchAppName, DatabaseURL: "sqlite:" + dbPath, ExecutorID: orchExecutorID,
		ApplicationVersion: orchAppVersion, Logger: lg,
	})
	if err != nil {
		return fmt.Errorf("orchestration database: %w", err)
	}
	dbos.RegisterWorkflow(dctx, vmbotOp, dbos.WithWorkflowName(orchWorkflow))
	q, err := dbos.RegisterQueue(dctx, orchQueue, dbos.WithPartitionConcurrency(1),
		dbos.WithQueueBasePollingInterval(50*time.Millisecond))
	if err != nil {
		_ = dbos.Shutdown(dctx, time.Second)
		return fmt.Errorf("orchestration queue: %w", err)
	}
	e := &engine{ctx: dctx, queue: q}
	c.engMu.Lock()
	c.eng = e // publish before Launch can recover workflow bodies
	c.engMu.Unlock()
	claimed = false // Close owns release; failed shutdown must retain the flock
	if err := dbos.Launch(dctx); err != nil {
		e.closeOnce.Do(func() { e.closeErr = dbos.Shutdown(dctx, time.Second) })
		if e.closeErr == nil {
			c.st.releaseEngine(c)
		}
		return fmt.Errorf("orchestration launch: %w", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} { // keep the files private whatever the umask
		_ = os.Chmod(dbPath+suffix, 0o600)
	}
	e.ready.Store(true)
	return nil
}

// Close stops the orchestration layer (graceful; in-flight work is replayed on
// the next start). It never touches VMs or terminals.
func (c *Controller) Close() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	e := c.engine()
	if e == nil {
		return nil
	}
	e.ready.Store(false)
	e.closeOnce.Do(func() { e.closeErr = dbos.Shutdown(e.ctx, 10*time.Second) })
	if e.closeErr == nil {
		c.st.releaseEngine(c)
	}
	return e.closeErr
}

// vmbotOp is the one registered workflow. Bodies are plain package-level code so
// DBOS can find them by name after a restart; the controller travels in the
// context.
func vmbotOp(ctx dbos.Context, in OpInput) (OpResult, error) {
	c, _ := ctx.Value(ctlKey{}).(*Controller)
	if c == nil {
		return OpResult{Code: codeFailed, Msg: "no controller"}, nil
	}
	switch in.Kind {
	case opCreate:
		return c.createFlow(ctx, in)
	case opDelete:
		return c.deleteFlow(ctx, in)
	case opTerminal:
		return c.terminalFlow(ctx, in)
	case opReconcile:
		return c.reconcileFlow(ctx, in)
	}
	return OpResult{Code: codeFailed, Msg: "unknown operation"}, nil
}

func step(ctx dbos.Context, name string, f func(context.Context) (OpResult, error)) (OpResult, error) {
	return dbos.RunAsStep(ctx, f, dbos.WithStepName(name), dbos.WithStepMaxRetries(0))
}

func toResult(s Status, err error) OpResult {
	if err == nil {
		return OpResult{Status: s}
	}
	r := OpResult{Status: s, Code: codeFailed}
	base := ErrFailed
	for code, sentinel := range map[string]error{
		codeUncertain: ErrUncertain, codeBackendFull: ErrBackendFull, codeNotRunning: ErrNotRunning,
		codeNoVM: ErrNoVM, codeStale: ErrStale,
	} {
		if errors.Is(err, sentinel) {
			r.Code, base = code, sentinel
		}
	}
	r.Msg = strings.TrimPrefix(err.Error(), base.Error())
	r.Msg = strings.TrimPrefix(r.Msg, ": ")
	return r
}

func (r OpResult) err() error {
	var base error
	switch r.Code {
	case codeOK:
		return nil
	case codeUncertain:
		base = ErrUncertain
	case codeBackendFull:
		base = ErrBackendFull
	case codeNotRunning:
		base = ErrNotRunning
	case codeNoVM:
		base = ErrNoVM
	case codeStale:
		base = ErrStale
	default:
		base = ErrFailed
	}
	if r.Msg != "" {
		return fmt.Errorf("%w: %s", base, r.Msg)
	}
	return base
}

// ---- submission ----

type opHandle = dbos.WorkflowHandle[OpResult]

func createID(vmID string) string             { return "create-" + vmID }
func deleteID(vmID string, seq uint64) string { return fmt.Sprintf("delete-%s-%d", vmID, seq) }

func (c *Controller) newRunID(kind, vmID string) (string, error) {
	t, err := randToken()
	if err != nil {
		return "", err
	}
	return kind + "-" + vmID + "-" + t[:12], nil
}

func activeStatus(s dbos.WorkflowStatusType) bool {
	return s == dbos.WorkflowStatusEnqueued || s == dbos.WorkflowStatusPending || s == dbos.WorkflowStatusDelayed
}

// lookup returns the existing workflow for an ID, if any.
func (c *Controller) lookup(id string) (opHandle, *dbos.WorkflowStatus) {
	e := c.engine()
	if e == nil {
		return nil, nil
	}
	h, err := dbos.RetrieveWorkflow[OpResult](e.ctx, id)
	if err != nil {
		return nil, nil
	}
	st, err := h.GetStatus()
	if err != nil {
		return nil, nil
	}
	return h, &st
}

func sameInput(got any, want OpInput) bool {
	var in OpInput
	switch v := got.(type) {
	case OpInput:
		in = v
	case string:
		if json.Unmarshal([]byte(v), &in) != nil {
			return false
		}
	default:
		b, err := json.Marshal(got)
		if err != nil || json.Unmarshal(b, &in) != nil {
			return false
		}
	}
	return in == want
}

// submit enqueues (idempotently, by workflow ID) and verifies that the execution
// DBOS handed back really carries our input: DBOS silently returns an old
// execution for a reused ID, which must never retarget a command.
func (c *Controller) submit(in OpInput, id string) (opHandle, error) {
	e := c.engine()
	if e == nil || !e.ready.Load() {
		return nil, fmt.Errorf("%w: orchestration not running", ErrFailed)
	}
	h, err := dbos.RunWorkflow(e.ctx, vmbotOp, in, dbos.WithQueue(e.queue),
		dbos.WithWorkflowID(id), dbos.WithQueuePartitionKey(in.VMID))
	if err != nil {
		c.logf("submit %s: %v", id, err)
		return nil, fmt.Errorf("%w: cannot submit operation", ErrFailed)
	}
	st, err := h.GetStatus()
	if err != nil || !sameInput(st.Input, in) {
		c.logf("submit %s: workflow ID collision or unreadable status (err=%v)", id, err)
		return nil, fmt.Errorf("%w: operation identity conflict", ErrFailed)
	}
	return h, nil
}

// wait waits for the outcome. A timeout leaves the workflow running; the caller
// then reports it as busy/uncertain from durable state.
func (c *Controller) wait(h opHandle, d time.Duration) (OpResult, error) {
	// SDK defaults to one-second sleeps, which can outlive a short query
	// budget even when the workflow has already completed. Poll within the
	// caller's budget without cancelling the durable workflow on timeout.
	poll := min(20*time.Millisecond, d/10)
	if poll <= 0 {
		return OpResult{}, ErrBusy
	}
	r, err := h.GetResult(dbos.WithHandleTimeout(d), dbos.WithHandlePollingInterval(poll))
	if err != nil {
		if st, serr := h.GetStatus(); serr == nil && activeStatus(st.Status) {
			return OpResult{}, ErrBusy
		}
		stStr := "unknown"
		if st, serr := h.GetStatus(); serr == nil {
			stStr = string(st.Status)
		}
		c.logf("workflow %s (%s): %v", h.GetWorkflowID(), stStr, err)
		return OpResult{}, fmt.Errorf("%w: operation did not complete", ErrFailed)
	}
	return r, nil
}

// ensureCreate submits (or re-attaches to) the create workflow of a record.
func (c *Controller) ensureCreate(rec VMRecord) (opHandle, error) {
	return c.submit(OpInput{Kind: opCreate, VMID: rec.ID}, createID(rec.ID))
}

// createFinished reports whether the create workflow of a record has concluded
// (any terminal status). Reconcile and its absence barrier must never run while
// Create may still be in flight.
func (c *Controller) createFinished(rec VMRecord) bool {
	_, st := c.lookup(createID(rec.ID))
	if st == nil {
		return false
	}
	switch st.Status {
	case dbos.WorkflowStatusSuccess, dbos.WorkflowStatusError, dbos.WorkflowStatusCancelled, dbos.WorkflowStatusMaxRecoveryAttemptsExceeded:
		return true
	default:
		return false // unreadable or unknown states are not proof of settlement
	}
}

// submitDelete makes the delete intent durable and returns the workflow to wait
// on. The deleting phase and Seq are persisted before anything is enqueued and
// the local terminal is revoked immediately (the queue position may be behind a
// long operation of the same VM). An attempt that already finished without
// removing the record is never replayed from the cache: Seq+1 is persisted and
// a new workflow ID is used.
func (c *Controller) submitDelete(rec VMRecord) (opHandle, VMRecord, error) {
	h, cur, err := c.persistAndSubmitDelete(rec)
	// Cleanup can wait on ttyd/SSH for seconds. Never hold the shared intent
	// gate while draining one VM; the workflow also revokes before its effect.
	if cur.ID != "" {
		c.term.Revoke(cur.ID)
	}
	return h, cur, err
}

func (c *Controller) persistAndSubmitDelete(rec VMRecord) (opHandle, VMRecord, error) {
	c.intentMu.Lock()
	defer c.intentMu.Unlock()
	var cur VMRecord
	if err := c.st.UpdateErr(func(s *State) error {
		found := false
		s.update(rec.ID, func(r *VMRecord) {
			found = true
			if r.Phase != PhaseDeleting {
				r.Phase = PhaseDeleting
			}
			if r.Seq == 0 {
				r.Seq = 1
			}
			cur = *r
		})
		if !found {
			return ErrNoVM
		}
		return nil
	}); err != nil {
		if errors.Is(err, ErrNoVM) {
			return nil, VMRecord{}, err
		}
		return nil, VMRecord{}, fmt.Errorf("%w: cannot persist state; nothing was changed", ErrFailed)
	}
	if _, st := c.lookup(deleteID(cur.ID, cur.Seq)); st != nil && !activeStatus(st.Status) {
		if err := c.st.UpdateErr(func(s *State) error {
			s.update(cur.ID, func(r *VMRecord) { r.Seq++; cur = *r })
			return nil
		}); err != nil {
			return nil, cur, fmt.Errorf("%w: cannot persist state", ErrFailed)
		}
	}
	h, err := c.submit(OpInput{Kind: opDelete, VMID: cur.ID, Seq: cur.Seq}, deleteID(cur.ID, cur.Seq))
	return h, cur, err
}

// reconcileVM runs one guarded reconcile workflow for a record and waits up to d.
// A record whose Create has not concluded is reported from durable state only.
func (c *Controller) reconcileVM(rec VMRecord, d time.Duration) (Status, error) {
	if rec.Phase == PhaseCreating {
		h, err := c.ensureCreate(rec)
		if err != nil {
			s := statusOf(&rec, nil)
			s.Uncertain = true
			return s, fmt.Errorf("%w", ErrUncertain)
		}
		// A terminal SDK error is not ongoing work. Never resubmit Create under
		// a fresh ID: reconcile the external result through Get/Delete instead.
		_, _ = c.wait(h, d)
		if !c.createFinished(rec) {
			s := statusOf(&rec, nil)
			s.Busy = true
			return s, nil
		}
	}
	id, err := c.newRunID(opReconcile, rec.ID)
	if err != nil {
		return statusOf(&rec, nil), fmt.Errorf("%w: random source", ErrFailed)
	}
	h, err := c.submit(OpInput{Kind: opReconcile, VMID: rec.ID}, id)
	if err != nil {
		return statusOf(&rec, nil), err
	}
	r, err := c.wait(h, d)
	if errors.Is(err, ErrBusy) {
		s := statusOf(&rec, nil)
		s.Busy = true
		return s, nil
	}
	if err != nil {
		return statusOf(&rec, nil), err
	}
	return r.Status, r.err()
}

// ---- workflow bodies ----

func (c *Controller) createFlow(ctx dbos.Context, in OpInput) (OpResult, error) {
	eff, err := step(ctx, "create-effect", func(sctx context.Context) (OpResult, error) {
		rec, ok := c.st.Snapshot().ByID(in.VMID)
		if !ok {
			return OpResult{Code: codeStale, Msg: "VM no longer owned"}, nil
		}
		if rec.Phase == PhaseDeleting {
			// delete intent persisted first: a recovered create never resurrects it
			return OpResult{Code: codeStale, Msg: "delete intent present", Status: statusOf(&rec, nil)}, nil
		}
		qctx, cancel := c.query(sctx)
		vm, gerr := c.lc.Get(qctx, rec.ID)
		cancel()
		if gerr == nil { // replay/recovery: adopt, never create twice
			return OpResult{Status: statusOf(&rec, &vm)}, nil
		}
		if !errors.Is(gerr, vmlib.ErrNotFound) {
			s := statusOf(&rec, nil)
			s.Uncertain = true
			return OpResult{Code: codeUncertain, Msg: "creation outcome unknown", Status: s}, nil
		}
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(sctx), c.cfg.OpTimeout)
		vm, cerr := c.lc.Create(cctx, vmlib.CreateRequest{ID: rec.ID, ImageID: c.cfg.ImageID, Profile: c.cfg.Profile,
			Labels: map[string]string{"app": "vmbot"}})
		ccancel()
		if cerr != nil {
			c.logf("create %s: %v", rec.ID, cerr)
			return toResult(c.classifyCreateFailure(sctx, rec, errors.Is(cerr, vmlib.ErrResourceExhausted))), nil
		}
		return OpResult{Status: statusOf(&rec, &vm)}, nil
	})
	if err != nil {
		return toResult(Status{}, fmt.Errorf("%w: %v", ErrFailed, err)), nil
	}
	if eff.Code != codeOK {
		return eff, nil
	}
	com, err := step(ctx, "commit-active", func(sctx context.Context) (OpResult, error) {
		rec, ok := c.st.Snapshot().ByID(in.VMID)
		if !ok {
			return OpResult{Code: codeStale, Msg: "VM no longer owned"}, nil
		}
		if rec.Phase == PhaseCreating {
			if uerr := c.setPhase(rec.ID, PhaseCreating, PhaseActive); uerr != nil {
				return OpResult{Code: codeUncertain, Msg: "cannot persist state", Status: eff.Status}, nil
			}
			rec.Phase = PhaseActive
		}
		s := eff.Status
		s.Phase = rec.Phase
		return OpResult{Status: s}, nil
	})
	if err != nil || com.Code != codeOK {
		if err != nil {
			return toResult(eff.Status, fmt.Errorf("%w: %v", ErrFailed, err)), nil
		}
		return com, nil
	}
	_, _ = step(ctx, "attach-terminal", func(sctx context.Context) (OpResult, error) {
		rec, ok := c.st.Snapshot().ByID(in.VMID)
		if ok && rec.Phase == PhaseActive && (com.Status.State == string(vmlib.StateRunning) || com.Status.State == "") {
			tctx, cancel := c.query(sctx)
			defer cancel()
			if terr := c.ensureTerminalLocked(tctx, rec); terr != nil {
				c.logf("terminal start failed for %s: %v", rec.ID, terr)
			}
		}
		return OpResult{}, nil
	})
	return com, nil
}

// classifyCreateFailure resolves an unknown Create outcome with Get; NotFound is
// resolved only by the delete barrier (ADR 0002).
func (c *Controller) classifyCreateFailure(ctx context.Context, rec VMRecord, backendFull bool) (Status, error) {
	qctx, cancel := c.query(ctx)
	defer cancel()
	vm, err := c.lc.Get(qctx, rec.ID)
	switch {
	case errors.Is(err, vmlib.ErrNotFound):
		if c.confirmGoneLocked(ctx, rec) {
			if backendFull {
				return Status{}, fmt.Errorf("%w: VM was not created", ErrBackendFull)
			}
			return Status{}, fmt.Errorf("%w: VM was not created", ErrFailed)
		}
		s := statusOf(&rec, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w: creation outcome unknown", ErrUncertain)
	case err != nil:
		s := statusOf(&rec, nil)
		s.Uncertain = true
		return s, fmt.Errorf("%w: creation outcome unknown", ErrUncertain)
	}
	_ = c.setPhase(rec.ID, PhaseCreating, PhaseActive)
	rec.Phase = PhaseActive
	return statusOf(&rec, &vm), fmt.Errorf("%w: VM exists but creation reported an error", ErrFailed)
}

func (c *Controller) deleteFlow(ctx dbos.Context, in OpInput) (OpResult, error) {
	intent, err := step(ctx, "delete-intent", func(sctx context.Context) (OpResult, error) {
		rec, ok := c.st.Snapshot().ByID(in.VMID)
		if !ok {
			return OpResult{Msg: "gone"}, nil // already removed by an earlier run of this intent
		}
		if rec.Phase != PhaseDeleting { // durable intent first (normally done by the command already)
			if uerr := c.setPhase(rec.ID, rec.Phase, PhaseDeleting); uerr != nil {
				return OpResult{Code: codeFailed, Msg: "cannot persist state; nothing was changed", Status: statusOf(&rec, nil)}, nil
			}
		}
		c.term.Revoke(rec.ID)
		return OpResult{Status: statusOf(&rec, nil)}, nil
	})
	if err != nil {
		return toResult(Status{}, fmt.Errorf("%w: %v", ErrFailed, err)), nil
	}
	if intent.Code != codeOK || intent.Msg == "gone" {
		return intent, nil
	}
	eff, err := step(ctx, "delete-effect", func(sctx context.Context) (OpResult, error) {
		rec, ok := c.st.Snapshot().ByID(in.VMID)
		if !ok {
			return OpResult{}, nil
		}
		r := rec
		r.Phase = PhaseDeleting
		dctx, cancel := context.WithTimeout(context.WithoutCancel(sctx), c.cfg.OpTimeout)
		res, derr := c.lc.Delete(dctx, rec.ID)
		cancel()
		if derr == nil && res.Deleted {
			return OpResult{}, nil
		}
		if derr != nil {
			c.logf("delete %s: %v", rec.ID, derr)
		}
		// Only a successful Delete releases the record; Get NotFound is no proof.
		qctx, qcancel := c.query(sctx)
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
			return toResult(s, fmt.Errorf("%w: VM retained, exit not proven", ErrFailed)), nil
		}
		return toResult(s, fmt.Errorf("%w: delete not confirmed", ErrFailed)), nil
	})
	if err != nil {
		return toResult(intent.Status, fmt.Errorf("%w: %v", ErrFailed, err)), nil
	}
	if eff.Code != codeOK {
		return eff, nil
	}
	return step(ctx, "commit-gone", func(sctx context.Context) (OpResult, error) {
		rec, ok := c.st.Snapshot().ByID(in.VMID)
		if !ok {
			return OpResult{Status: Status{Generation: intent.Status.Generation, VMID: in.VMID, Name: intent.Status.Name}}, nil
		}
		if derr := c.drop(rec.ID); derr != nil {
			r := rec
			r.Phase = PhaseDeleting
			return toResult(statusOf(&r, nil), fmt.Errorf("%w: cannot persist state", ErrUncertain)), nil
		}
		return OpResult{Status: Status{Generation: rec.Generation, VMID: rec.ID, Name: rec.Label}}, nil
	})
}

func (c *Controller) terminalFlow(ctx dbos.Context, in OpInput) (OpResult, error) {
	return step(ctx, "authorize-terminal", func(sctx context.Context) (OpResult, error) {
		rec, ok := c.st.Snapshot().ByID(in.VMID)
		if !ok {
			return OpResult{Code: codeNoVM, Msg: "VM no longer owned"}, nil
		}
		st, err := c.reconcileLocked(sctx, rec) // re-reads durable phase: deleting never grants
		if err != nil {
			return toResult(st, err), nil
		}
		if !st.Present {
			return OpResult{Code: codeNoVM}, nil
		}
		cur, ok := c.st.Snapshot().ByID(in.VMID)
		if !ok || cur.Phase != PhaseActive || st.State != string(vmlib.StateRunning) {
			return OpResult{Code: codeNotRunning, Status: st}, nil
		}
		qctx, cancel := c.query(sctx)
		defer cancel()
		if err := c.ensureTerminalLocked(qctx, cur); err != nil {
			c.logf("terminal start failed for %s: %v", cur.ID, err)
			return OpResult{Code: codeFailed, Msg: "terminal backend unavailable", Status: st}, nil
		}
		return OpResult{Status: st}, nil
	})
}

func (c *Controller) reconcileFlow(ctx dbos.Context, in OpInput) (OpResult, error) {
	return step(ctx, "reconcile", func(sctx context.Context) (OpResult, error) {
		rec, ok := c.st.Snapshot().ByID(in.VMID)
		if !ok {
			return OpResult{}, nil
		}
		return toResult(c.reconcileLocked(sctx, rec)), nil
	})
}
