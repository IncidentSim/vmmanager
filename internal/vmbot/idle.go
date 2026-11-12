package vmbot

// SSH-idle expiry (docs/adr/0008-ssh-idle-expiry.md).
//
// This file is an observer, not a lifecycle controller. It counts established
// guest SSH connections and, after a VM has been continuously observed with none
// for longer than IdleTimeout, admits one ordinary owner-authored Delete (the
// same mechanism as the TTL expiry Delete). It never frees disk or slots, stops a
// VMM or touches credentials or terminals: the per-VM owner executes the Delete
// serialized with every other command through begin -> effect -> project.
//
// Only a clean, trustworthy "zero" is ever acted on. A failed, unsupported or
// ambiguous observation, a VM that is not running, or any unfinished command on
// the VM clears the window: absence of evidence never deletes a VM. The window
// lives in memory only and restarts after a process restart.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

const (
	// DefaultIdleTimeout is the continuous no-SSH period after which a VM is deleted.
	DefaultIdleTimeout = 600 * time.Second
	// DefaultIdlePoll is the observation period.
	DefaultIdlePoll = 30 * time.Second
	// idleProbeTimeout bounds one VM observation (state, metadata and SSH probe).
	idleProbeTimeout = 15 * time.Second

	// idleOpPrefix/idleKeyPrefix identify the owner-authored idle Delete. The key
	// contains a space, which request keys can never contain, so no caller can
	// collide with or replay it.
	idleOpPrefix  = "op-idle-"
	idleKeyPrefix = "idle expire:"
)

// ConnectionCounter reports the number of established SSH connections to a guest,
// excluding the observer's own connection. An error means "unknown", never zero.
type ConnectionCounter interface {
	Count(ctx context.Context, vmID, host, user string) (int, error)
}

// idleTracker holds the per-VM instant from which zero connections have been
// observed continuously. Absent means no clean zero is on record.
type idleTracker struct {
	mu   sync.Mutex
	zero map[string]time.Time
}

func (t *idleTracker) reset(id string) {
	t.mu.Lock()
	delete(t.zero, id)
	t.mu.Unlock()
}

// observeZero records a clean zero and returns when the continuous run began.
func (t *idleTracker) observeZero(id string, now time.Time) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.zero == nil {
		t.zero = map[string]time.Time{}
	}
	since, ok := t.zero[id]
	if !ok {
		since = now
		t.zero[id] = now
	}
	return since
}

func (t *idleTracker) prune(live map[string]bool) {
	t.mu.Lock()
	for id := range t.zero {
		if !live[id] {
			delete(t.zero, id)
		}
	}
	t.mu.Unlock()
}

func (c *Controller) idleEnabled() bool {
	return c.cfg.IdleProbe != nil && c.cfg.IdleTimeout > 0
}

func (c *Controller) startIdleMonitor() {
	if !c.idleEnabled() {
		return
	}
	poll := c.cfg.IdlePoll
	if poll <= 0 {
		poll = DefaultIdlePoll
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.idleStop, c.idleDone = cancel, done
	go func() {
		defer close(done)
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.idleTick(ctx)
			}
		}
	}()
}

func (c *Controller) stopIdleMonitor() {
	if c.idleStop != nil {
		c.idleStop()
		<-c.idleDone
		c.idleStop, c.idleDone = nil, nil
	}
}

// idleTick observes every VM once, sequentially (bounded work per poll).
func (c *Controller) idleTick(ctx context.Context) {
	if !c.idleEnabled() {
		return
	}
	snap := c.st.Snapshot()
	live := map[string]bool{}
	for _, v := range snap.VMs {
		live[v.ID] = true
	}
	c.idle.prune(live)
	if !snap.Control {
		return // no owner workflows: nothing could serialize a Delete
	}
	for _, v := range snap.VMs {
		if ctx.Err() != nil {
			return
		}
		c.idleCheck(ctx, v.ID)
	}
}

func idleEligible(r VMRecord) bool {
	return r.OwnerUserID > 0 && (r.Phase == PhaseActive || r.Phase == PhaseDeleting)
}

func unfinishedFor(s State, vmID string) bool {
	for _, o := range s.Operations {
		if o.VMID == vmID && (o.Status == "pending" || o.Status == "running") {
			return true
		}
	}
	return false
}

// idleObserve returns the number of other established SSH connections, or an
// error when the VM cannot be cleanly observed right now.
func (c *Controller) idleObserve(ctx context.Context, vmID string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, idleProbeTimeout)
	defer cancel()
	vm, err := c.lc.Get(ctx, vmID)
	if err != nil {
		return 0, err
	}
	if vm.State != vmlib.StateRunning {
		return 0, fmt.Errorf("VM is %s", vm.State)
	}
	info, err := c.lc.SSHInfo(ctx, vmID)
	if err != nil {
		return 0, err
	}
	// Host-local, backend-derived target only; never the public endpoint.
	if info.InternalTarget == nil || info.InternalTarget.Port != 22 || info.User == "" {
		return 0, fmt.Errorf("no internal SSH target")
	}
	return c.cfg.IdleProbe.Count(ctx, vmID, info.InternalTarget.Host, info.User)
}

func (c *Controller) idleCheck(ctx context.Context, vmID string) {
	snap := c.st.Snapshot()
	r, ok := snap.ByID(vmID)
	if !ok || !idleEligible(r) || unfinishedFor(snap, vmID) {
		c.idle.reset(vmID)
		return
	}
	n, err := c.idleObserve(ctx, vmID)
	if err != nil || n != 0 {
		c.idle.reset(vmID)
		return
	}
	now := c.now()
	if now.Sub(c.idle.observeZero(vmID, now)) <= c.cfg.IdleTimeout {
		return
	}
	// Threshold passed: one more clean observation immediately before admission.
	if n, err := c.idleObserve(ctx, vmID); err != nil || n != 0 {
		c.idle.reset(vmID)
		return
	}
	op, ok := c.admitIdleDelete(vmID)
	c.idle.reset(vmID) // the next window starts only after this command settled
	if ok {
		c.dispatch(&op)
	}
}

// admitIdleDelete atomically re-checks eligibility and that nothing is unfinished
// for the VM, then records the owner-authored Delete. Attempts are numbered, so a
// failed ordinary Delete is retried under a new operation for the same VM.
func (c *Controller) admitIdleDelete(vmID string) (ControlOperation, bool) {
	var op ControlOperation
	err := c.st.UpdateErr(func(s *State) error {
		r, ok := s.ByID(vmID)
		if !ok || !idleEligible(r) || unfinishedFor(*s, vmID) {
			return nil
		}
		attempt := 0
		for _, o := range s.Operations {
			if o.VMID == vmID && strings.HasPrefix(o.ID, idleOpPrefix) {
				attempt++
			}
		}
		key := fmt.Sprintf("%s%s:%d", idleKeyPrefix, vmID, attempt+1)
		sum := sha256.Sum256([]byte(key))
		op = ControlOperation{ID: fmt.Sprintf("%s%s-%d", idleOpPrefix, strings.TrimPrefix(vmID, "bot-"), attempt+1), OwnerUserID: r.OwnerUserID,
			Key: key, Digest: hex.EncodeToString(sum[:]), Kind: "delete", VMID: vmID, Status: "pending", Exec: execOwner}
		s.Operations = append(s.Operations, op)
		return nil
	})
	if err != nil {
		c.logf("idle expiry for %s: %v", vmID, err)
		return ControlOperation{}, false
	}
	return op, op.ID != ""
}

// idleStillConfirmed runs inside the owner's checkpointed begin step for a
// pending idle Delete: the VM must still be cleanly observed with no connection.
func (c *Controller) idleStillConfirmed(vmID string) bool {
	if !c.idleEnabled() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), idleProbeTimeout)
	defer cancel()
	n, err := c.idleObserve(ctx, vmID)
	return err == nil && n == 0
}
