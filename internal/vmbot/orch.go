package vmbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	_ "github.com/dbos-inc/dbos-transact-golang/dbos/driver/sqlite" // pure-Go SQLite system database
)

// This file wires the DBOS engine. Control stores (the cmd/vmapi path) are driven
// only by the per-VM owner workflows in owner.go. The "vmbot-op" workflow name
// and the "vmops" queue remain registered solely so that executions written by
// earlier builds into the same system database can be recovered (and retired)
// instead of being orphaned. Domain truth stays in state.json; the SQLite system
// database is the durable execution log (owner command consumption, child
// results, replay).

const (
	orchAppName    = "vmbot"
	orchExecutorID = "vmbot" // fixed: a second executor would strand or re-run work
	// orchAppVersion is a constant, not the binary hash, so pending workflows are
	// recovered after a rebuild. Bump it only together with a drain/migration plan.
	orchAppVersion = "vmbot-owner-1"
	orchQueue      = "vmops"
	orchWorkflow   = "vmbot-op"
	orchSubdir     = "orchestration"
	orchDBName     = "dbos.db"
)

// OpInput is the input shape of the retired "vmbot-op" workflow. It must keep
// decoding what earlier builds stored: identifiers only, never a token or link.
type OpInput struct {
	Kind string
	VMID string
	Seq  uint64
}

// OpResult is the output shape of the retired "vmbot-op" workflow.
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
	if snap := c.st.Snapshot(); snap.Control {
		if err := checkHandoff(snap); err != nil {
			return err
		}
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
	dbos.RegisterWorkflow(dctx, vmOwner, dbos.WithWorkflowName(ownerWorkflowName))
	dbos.RegisterWorkflow(dctx, vmEffect, dbos.WithWorkflowName(effectWorkflowName))
	dbos.RegisterWorkflow(dctx, imageDelete, dbos.WithWorkflowName(imageWorkflowName))
	q, err := dbos.RegisterQueue(dctx, orchQueue, dbos.WithPartitionConcurrency(1),
		dbos.WithQueueBasePollingInterval(50*time.Millisecond))
	if err != nil {
		_ = dbos.Shutdown(dctx, time.Second)
		return fmt.Errorf("orchestration queue: %w", err)
	}
	e := &engine{ctx: dctx, queue: q}
	// Arm the one-time terminal rebuild for every VM stored now (see owner.go).
	for _, v := range c.st.Snapshot().VMs {
		c.rebuild.Store(v.ID, true)
	}
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

// vmbotOp is the retirement stub for the former single-tenant workflow. It does
// nothing but record a failed result, so a leftover execution of that name
// completes instead of being orphaned. It never touches state, VMs or terminals.
func vmbotOp(_ dbos.Context, _ OpInput) (OpResult, error) {
	return OpResult{Code: codeFailed, Msg: "legacy workflow retired by control migration"}, nil
}

const codeFailed = "failed"
