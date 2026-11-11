package vmbot

// The sequential durable VM owner (docs/rfc-vm-owner.md).
//
// One long-lived DBOS workflow per immutable VM ID is the only author of that
// VM's applied lifecycle: it receives durable commands (Recv), waits for exactly
// one finite effect child workflow at a time, and writes the outcome to the
// store projection. Children perform effects and return typed outcomes; they
// never write application state. Admission (control.go) only reserves
// identities and delivers a command: it never changes the applied phase or
// revokes access.
//
// Replay compatibility: workflow names and step names below are pinned. Any
// change to the order or number of durable calls in vmOwner must be guarded by
// dbos.Patch, because an owner may outlive a release (see docs/control-api.md).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

const (
	ownerWorkflowName  = "vmbot-vm-owner"
	effectWorkflowName = "vmbot-vm-effect"
	imageWorkflowName  = "vmbot-image-delete"
	ownerCmdTopic      = "cmd"
	// ownerIdle bounds one Recv; a timeout only loops, and costs one history row.
	ownerIdle = 6 * time.Hour
	execOwner = "owner" // ControlOperation.Exec of every operation the owner model runs
)

// ErrOwnerUnavailable: the VM's owner workflow is not running (terminal ERROR,
// cancelled, or never started). A command is never reported accepted for it.
var ErrOwnerUnavailable = errors.New("VM owner workflow is not running")

// ownerWorkflowID is the stable owner identity: one per immutable VM ID.
func ownerWorkflowID(vmID string) string { return "vmbot-owner-" + vmID }
func effectWorkflowID(vmID, opID string) string {
	return ownerWorkflowID(vmID) + "." + opID
}
func imageWorkflowID(opID string) string { return "image-delete-" + opID }

// OwnerIn carries identities only. CreateOpID is empty for an adopted VM.
type OwnerIn struct {
	VMID         string
	CreateOpID   string
	CreateDigest string
}
type OwnerOut struct {
	VMID  string
	Phase string
}

// OwnerCmd is the durable command. Kind and Digest repeat the request identity
// stored at admission, so a message deduplicated by key can never stand in for
// a different request.
type OwnerCmd struct {
	OpID, Kind, Digest string
}

// EffectIn identifies one finite child. It carries no logical state.
type EffectIn struct {
	VMID, OpID, Kind, Digest string
}

// EffectOut is the typed outcome of a child. An expected failure is a value.
type EffectOut struct {
	OpID      string
	Code      string // "" success, else the stable operation error code
	State     string // observed vmlib state, when known
	Exists    bool   // the VM is known to exist (also after a failed create)
	Resources *ControlResources
}

type ownerPlan struct {
	Run  bool
	Note string
}

type ownerFinal struct{ Gone bool }

func ownerStep[T any](ctx dbos.Context, name string, retries int, f func(context.Context) (T, error)) (T, error) {
	return dbos.RunAsStep(ctx, f, dbos.WithStepName(name), dbos.WithStepMaxRetries(retries),
		dbos.WithStepBaseInterval(100*time.Millisecond))
}

func controllerOf(ctx dbos.Context) *Controller {
	c, _ := ctx.Value(ctlKey{}).(*Controller)
	return c
}

// vmOwner is the long-lived per-VM workflow. All control flow depends only on
// checkpointed values (Recv payloads, step and child outputs).
func vmOwner(ctx dbos.Context, in OwnerIn) (OwnerOut, error) {
	c := controllerOf(ctx)
	if c == nil {
		return OwnerOut{}, errors.New("no controller")
	}
	if in.CreateOpID != "" {
		if _, err := c.ownerHandle(ctx, in.VMID, OwnerCmd{OpID: in.CreateOpID, Kind: "create", Digest: in.CreateDigest}); err != nil {
			return OwnerOut{}, err
		}
	}
	for {
		c.rebuildTerminal(in.VMID) // local projection, once per process; see Start
		cmd, err := dbos.Recv[OwnerCmd](ctx, ownerCmdTopic, ownerIdle)
		if err != nil {
			var de *dbos.Error
			if errors.As(err, &de) && de.Code == dbos.ErrorCodeTimeout {
				continue
			}
			return OwnerOut{}, err
		}
		if cmd.Kind == "create" || cmd.Kind == "delete_image" || cmd.Kind == "" {
			// Not a command this owner can run: a protocol violation, not a
			// recoverable outcome. It makes the owner visibly ERROR (retained).
			return OwnerOut{}, fmt.Errorf("unsupported owner command %q", cmd.Kind)
		}
		if cmd.Kind == "delete" {
			// Admission order is the durable store order. Anything admitted before
			// this Delete whose own message was delayed or never delivered runs
			// first, so Delete never skips accepted work. Admission bounds this
			// to one command; their later messages find them finished.
			prior, err := ownerStep(ctx, "prior", 3, func(context.Context) ([]OwnerCmd, error) { return c.priorUnfinished(in.VMID, cmd.OpID), nil })
			if err != nil {
				return OwnerOut{}, err
			}
			for _, p := range prior {
				if _, err := c.ownerHandle(ctx, in.VMID, p); err != nil {
					return OwnerOut{}, err
				}
			}
		}
		gone, err := c.ownerHandle(ctx, in.VMID, cmd)
		if err != nil {
			return OwnerOut{}, err
		}
		if gone {
			return OwnerOut{VMID: in.VMID, Phase: "deleted"}, nil
		}
	}
}

// ownerHandle runs one command to its recorded outcome: begin (validate, record
// "running", apply the transitional phase) -> one child -> project (the only
// application-state write for the outcome). Expected failures never return an
// error; an error here is an unexpected owner failure.
func (c *Controller) ownerHandle(ctx dbos.Context, vmID string, cmd OwnerCmd) (bool, error) {
	plan, err := ownerStep(ctx, "begin", 3, func(context.Context) (ownerPlan, error) { return c.beginOp(vmID, cmd) })
	if err != nil {
		return false, err
	}
	if !plan.Run {
		c.logf("owner %s: command %s not run: %s", vmID, cmd.OpID, plan.Note)
		return false, nil
	}
	in := EffectIn{VMID: vmID, OpID: cmd.OpID, Kind: cmd.Kind, Digest: cmd.Digest}
	out := EffectOut{OpID: cmd.OpID, Code: "unavailable"}
	h, err := dbos.RunWorkflow(ctx, vmEffect, in, dbos.WithWorkflowID(effectWorkflowID(vmID, cmd.OpID)))
	if err == nil {
		var cerr error
		out, cerr = h.GetResult(dbos.WithHandlePollingInterval(20 * time.Millisecond))
		err = cerr
		if cerr == nil && out.OpID != cmd.OpID {
			err = errors.New("child result for another command")
		}
	}
	if ctx.Err() != nil {
		// Shutdown or cancellation of the OWNER interrupted the wait. That is not a
		// child outcome: return it unrecorded (the SDK requires prompt return) and
		// let recovery resume the command from its checkpoints.
		return false, ctx.Err()
	}
	if err != nil {
		// A failed child is an outcome, never a poisoned owner.
		c.logf("owner %s: child for %s failed: %v", vmID, cmd.OpID, err)
		out = EffectOut{OpID: cmd.OpID, Code: "unavailable"}
	}
	fin, err := ownerStep(ctx, "project", 3, func(context.Context) (ownerFinal, error) { return c.applyOutcome(vmID, cmd, out) })
	if err != nil {
		return false, err
	}
	return fin.Gone, nil
}

// priorUnfinished lists the VM's unfinished commands admitted before opID, in
// admission (store) order. Create is carried by the owner input, not listed.
func (c *Controller) priorUnfinished(vmID, opID string) []OwnerCmd {
	var out []OwnerCmd
	for _, o := range c.st.Snapshot().Operations {
		if o.ID == opID {
			break
		}
		if o.VMID == vmID && o.Exec == execOwner && (o.Status == "pending" || o.Status == "running") && o.Kind != "create" && o.Kind != "delete_image" {
			out = append(out, OwnerCmd{OpID: o.ID, Kind: o.Kind, Digest: o.Digest})
		}
	}
	return out
}

// vmEffect is the finite child: effects only. It reads the store for identity
// inputs but never writes it.
func vmEffect(ctx dbos.Context, in EffectIn) (EffectOut, error) {
	c := controllerOf(ctx)
	if c == nil {
		return EffectOut{OpID: in.OpID, Code: "unavailable"}, nil
	}
	return ownerStep(ctx, "effect", 0, func(sctx context.Context) (EffectOut, error) { return c.runEffect(sctx, in), nil })
}

func controlErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, vmlib.ErrAlreadyExists), errors.Is(err, ErrControlConflict):
		return "conflict"
	case errors.Is(err, vmlib.ErrResourceExhausted):
		return "capacity"
	case errors.Is(err, vmlib.ErrInvalidArgument):
		return "invalid_argument"
	}
	return "unavailable"
}

// beginOp validates the command against the store and records that it runs. It
// is idempotent: a replay finds the operation "running" and proceeds again.
func (c *Controller) beginOp(vmID string, cmd OwnerCmd) (ownerPlan, error) {
	plan := ownerPlan{}
	err := c.st.UpdateErr(func(s *State) error {
		oi := -1
		for i := range s.Operations {
			if s.Operations[i].ID == cmd.OpID {
				oi = i
			}
		}
		if oi < 0 {
			plan.Note = "unknown operation"
			return nil
		}
		op := &s.Operations[oi]
		if op.Digest != cmd.Digest || op.Kind != cmd.Kind || (op.Kind != "delete_image" && op.VMID != vmID) {
			plan.Note = "command does not match the admitted request"
			return nil
		}
		if op.Status == "succeeded" || op.Status == "failed" {
			plan.Note = "already finished"
			return nil
		}
		reject := func(code string) {
			op.Status, op.ErrorCode = "failed", code
			plan.Note = "rejected: " + code
		}
		if op.Kind == "delete_image" {
			op.Status, plan.Run = "running", true
			return nil
		}
		r, ok := s.ByID(vmID)
		if !ok || r.OwnerUserID != op.OwnerUserID {
			reject("unavailable")
			return nil
		}
		from, to := "", ""
		// A "running" op was begun by an earlier execution of this very step whose
		// checkpoint was lost: its transitional phase is its own, so resume it.
		resumed := func(phase string) bool { return op.Status == "running" && r.Phase == phase }
		switch op.Kind {
		case "create":
			if r.Phase != PhaseCreating {
				reject("conflict")
				return nil
			}
		case "stop":
			if r.Phase != PhaseActive && r.Phase != "stopped" && r.Phase != "committed" && r.Phase != "stopping" {
				reject("conflict")
				return nil
			}
			from, to = PhaseActive, "stopping"
		case "commit":
			if r.Phase != PhaseActive && r.Phase != "stopped" && r.Phase != "committed" && !resumed("committing") {
				reject("conflict")
				return nil
			}
			from, to = r.Phase, "committing"
		case "terminal":
			if r.Phase != PhaseActive || !c.ControlAllowed(r.OwnerUserID) {
				reject("conflict")
				return nil
			}
		case "delete":
			from, to = r.Phase, PhaseDeleting
		default:
			reject("invalid_argument")
			return nil
		}
		if to != "" && r.Phase == from {
			s.update(vmID, func(v *VMRecord) { v.Phase = to })
		}
		op.Status, plan.Run = "running", true
		return nil
	})
	return plan, err
}

// runEffect performs the external effect of one command. It never writes the
// store. Stop/Commit/Delete revoke terminal access here, when they actually run.
func (c *Controller) runEffect(sctx context.Context, in EffectIn) EffectOut {
	out := EffectOut{OpID: in.OpID}
	snap := c.st.Snapshot()
	var op ControlOperation
	for _, o := range snap.Operations {
		if o.ID == in.OpID {
			op = o
		}
	}
	r, haveVM := snap.ByID(in.VMID)
	if op.ID == "" || op.Digest != in.Digest || op.Kind != in.Kind || (in.Kind != "delete_image" && (!haveVM || r.OwnerUserID != op.OwnerUserID)) {
		out.Code = "unavailable"
		return out
	}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(sctx), c.cfg.OpTimeout)
	defer cancel()
	var err error
	var vm vmlib.VM
	switch in.Kind {
	case "create":
		vm, err = c.lc.Get(qctx, r.ID)
		if err == nil && vm.State != vmlib.StateRunning {
			// A prepared workspace (crash between Prepare and Start) is not a
			// running guest. Retain it unchanged; only an explicit Delete frees it.
			out.Exists, out.State = true, string(vm.State)
			out.Code = "unavailable"
			return out
		}
		if errors.Is(err, vmlib.ErrNotFound) {
			req := vmlib.CreateRequest{ID: r.ID, ImageID: r.ImageID, Profile: c.cfg.Profile, SSHPublicKey: r.SSHPublicKey}
			if r.Resources != nil {
				req.Profile = ""
				req.Resources = &vmlib.VMResources{VCPU: r.Resources.VCPU, MemMiB: r.Resources.MemoryMiB}
			}
			vm, err = c.lc.Create(qctx, req)
			if err != nil {
				// Resolve an ambiguous failure with Get only: a VM that exists is
				// projected as such; a record whose Create failed is otherwise kept
				// (occupancy retained) until an explicit Delete.
				if got, gerr := c.lc.Get(qctx, r.ID); gerr == nil {
					out.Exists, out.State = true, string(got.State)
					out.Resources = resourcesOf(got)
				}
			}
		}
	case "stop":
		c.term.Revoke(r.ID)
		vm, err = c.lc.Stop(qctx, r.ID)
	case "commit":
		c.term.Revoke(r.ID)
		// Never adopt an unknown image on AlreadyExists: even a random collision
		// cannot confer ownership. Ambiguous publication retains the private intent.
		if _, err = c.lc.Stop(qctx, r.ID); err == nil {
			_, err = c.lc.Commit(qctx, r.ID, op.ImageID)
		}
	case "delete":
		c.term.Revoke(r.ID)
		var res vmlib.DeleteResult
		if res, err = c.lc.Delete(qctx, r.ID); err == nil && !res.Deleted {
			err = ErrUncertain
		}
	case "terminal":
		if !c.ControlAllowed(r.OwnerUserID) || r.Phase != PhaseActive {
			err = ErrNotRunning
			break
		}
		err = c.ensureTerminalLocked(qctx, r)
	case "delete_image":
		// Only a durably fenced, owned, unreferenced private image is ever sent to
		// the backend.
		if !c.deletableImage(snap, op) {
			err = ErrControlConflict
			break
		}
		err = c.lc.DeleteImage(qctx, op.ImageID)
	default:
		err = vmlib.ErrInvalidArgument
	}
	if err == nil && in.Kind == "create" && vm.State != vmlib.StateRunning {
		err = ErrUncertain
		out.Exists, out.State = true, string(vm.State)
	}
	out.Code = controlErrorCode(err)
	if err == nil && vm.ID != "" {
		out.Exists, out.State, out.Resources = true, string(vm.State), resourcesOf(vm)
	}
	if err != nil {
		c.logf("effect %s %s: %v", in.Kind, in.VMID, err)
	}
	return out
}

func resourcesOf(vm vmlib.VM) *ControlResources {
	if vm.Resources.VCPU == 0 {
		return nil
	}
	return &ControlResources{vm.Resources.VCPU, vm.Resources.MemMiB}
}

// applyOutcome is the single writer of an outcome: operation status and the VM
// projection change in one atomic store update, and repeating it is harmless.
func (c *Controller) applyOutcome(vmID string, cmd OwnerCmd, out EffectOut) (ownerFinal, error) {
	if c.testBeforeProject != nil {
		c.testBeforeProject(cmd)
	}
	fin := ownerFinal{}
	err := c.st.UpdateErr(func(s *State) error {
		for n := range s.Operations {
			o := &s.Operations[n]
			if o.ID != cmd.OpID {
				continue
			}
			if o.Status == "succeeded" || o.Status == "failed" {
				_, ok := s.ByID(vmID)
				fin.Gone = !ok
				return nil // projected by an earlier execution of this step
			}
			o.Status = "succeeded"
			if out.Code != "" {
				o.Status, o.ErrorCode = "failed", out.Code
			}
		}
		if out.Code != "" {
			// Failures keep the transitional phase: occupancy and the access fence are
			// retained until an explicit follow-up command, as before.
			if cmd.Kind == "create" {
				if out.Exists && out.State == string(vmlib.StateRunning) {
					s.update(vmID, func(v *VMRecord) { v.Phase, v.RemoteState = PhaseActive, out.State })
				}
			}
			return nil
		}
		switch cmd.Kind {
		case "create":
			s.update(vmID, func(v *VMRecord) {
				if v.Phase == PhaseCreating {
					v.Phase = PhaseActive
				}
				v.RemoteState = out.State
				if v.Resources == nil {
					v.Resources = out.Resources
				}
			})
		case "stop":
			s.update(vmID, func(v *VMRecord) {
				if v.Phase == "stopping" {
					v.Phase = "stopped"
				}
				v.RemoteState = out.State
			})
		case "commit":
			s.update(vmID, func(v *VMRecord) {
				if v.Phase == "committing" {
					v.Phase = "committed"
				}
				v.RemoteState = "committed"
			})
			var img string
			for _, o := range s.Operations {
				if o.ID == cmd.OpID {
					img = o.ImageID
				}
			}
			for n := range s.Images {
				if s.Images[n].ID == img {
					s.Images[n].Phase = "ready"
				}
			}
		case "delete":
			s.remove(vmID)
			fin.Gone = true
		case "delete_image":
			var img string
			for _, o := range s.Operations {
				if o.ID == cmd.OpID {
					img = o.ImageID
				}
			}
			keep := s.Images[:0]
			for _, i := range s.Images {
				if i.ID != img {
					keep = append(keep, i)
				}
			}
			s.Images = keep
		}
		return nil
	})
	return fin, err
}

// imageDelete is the separate shared-image deletion workflow: coordinated with
// image references at admission, never a message to an arbitrary VM owner.
func imageDelete(ctx dbos.Context, in EffectIn) (EffectOut, error) {
	c := controllerOf(ctx)
	if c == nil {
		return EffectOut{OpID: in.OpID, Code: "unavailable"}, nil
	}
	cmd := OwnerCmd{OpID: in.OpID, Kind: "delete_image", Digest: in.Digest}
	plan, err := ownerStep(ctx, "begin", 3, func(context.Context) (ownerPlan, error) { return c.beginOp("", cmd) })
	if err != nil || !plan.Run {
		return EffectOut{OpID: in.OpID, Code: "unavailable"}, err
	}
	out, err := ownerStep(ctx, "effect", 0, func(sctx context.Context) (EffectOut, error) { return c.runEffect(sctx, in), nil })
	if err != nil {
		return out, err
	}
	_, err = ownerStep(ctx, "project", 3, func(context.Context) (ownerFinal, error) { return c.applyOutcome("", cmd, out) })
	return out, err
}

// ---- delivery and liveness ----

func ownerAlive(s dbos.WorkflowStatusType) bool {
	return s == dbos.WorkflowStatusPending || s == dbos.WorkflowStatusEnqueued
}

// ownerState reports the persisted owner workflow status ("" when none exists).
func (c *Controller) ownerState(vmID string) (dbos.WorkflowStatusType, error) {
	e := c.engine()
	if e == nil || !e.ready.Load() {
		return "", fmt.Errorf("%w: orchestration not running", ErrFailed)
	}
	ws, err := dbos.ListWorkflows(e.ctx, dbos.WithFilterWorkflowIDs(ownerWorkflowID(vmID)),
		dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		return "", err
	}
	if len(ws) == 0 {
		return "", nil
	}
	return ws[0].Status, nil
}

// startOwner idempotently starts (or re-attaches to) the owner of a VM.
func (c *Controller) startOwner(in OwnerIn) error {
	e := c.engine()
	if e == nil || !e.ready.Load() {
		return fmt.Errorf("%w: orchestration not running", ErrFailed)
	}
	_, err := dbos.RunWorkflow(e.ctx, vmOwner, in, dbos.WithWorkflowID(ownerWorkflowID(in.VMID)))
	return err
}

// deliver durably sends one command. Success proves delivery only, never
// processing. A finished or failed owner is refused: DBOS would accept the
// message and never consume it. The status check is advisory (an owner can end
// right after it); the operation then stays visibly unfinished.
func (c *Controller) deliver(o ControlOperation) error {
	e := c.engine()
	if e == nil || !e.ready.Load() {
		return fmt.Errorf("%w: orchestration not running", ErrFailed)
	}
	if o.Kind == "delete_image" {
		_, err := dbos.RunWorkflow(e.ctx, imageDelete, EffectIn{OpID: o.ID, Kind: o.Kind, Digest: o.Digest}, dbos.WithWorkflowID(imageWorkflowID(o.ID)))
		return err
	}
	st, err := c.ownerState(o.VMID)
	if err != nil {
		return err
	}
	if !ownerAlive(st) {
		return ErrOwnerUnavailable
	}
	return dbos.Send(e.ctx, ownerWorkflowID(o.VMID), OwnerCmd{OpID: o.ID, Kind: o.Kind, Digest: o.Digest}, ownerCmdTopic, dbos.WithIdempotencyKey(o.ID))
}

// rebuildTerminal re-attaches a running VM's terminal after a process restart.
// Terminal processes are local, rebuildable projections, not checkpoints. It is
// armed only for VMs present at Start and runs inside the owner, so it is
// serialized with that VM's effects; the usual live-state fence still applies.
func (c *Controller) rebuildTerminal(vmID string) {
	if _, ok := c.rebuild.Load(vmID); !ok {
		return
	}
	r, ok := c.st.Snapshot().ByID(vmID)
	if !ok || (r.Phase != PhaseActive && r.Phase != "stopped" && r.Phase != "committed") {
		c.rebuild.Delete(vmID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.QueryTimeout)
	defer cancel()
	vm, err := c.lc.Get(ctx, vmID)
	if err != nil {
		if errors.Is(err, vmlib.ErrNotFound) {
			c.rebuild.Delete(vmID)
		}
		return // transient: retried at the next owner wake-up
	}
	c.rebuild.Delete(vmID)
	// Bootstrap observation of a settled (possibly sparse legacy) record: fill
	// only EMPTY public metadata, never identity, token, owner, name or phase,
	// and never recreate a record that was deleted meanwhile (update is a no-op).
	if serr := c.st.Update(func(s *State) {
		s.update(vmID, func(v *VMRecord) {
			if v.ImageID == "" {
				v.ImageID = vm.ImageID
			}
			if v.Resources == nil {
				v.Resources = resourcesOf(vm)
			}
			if v.RemoteState == "" {
				v.RemoteState = string(vm.State)
			}
		})
	}); serr != nil {
		c.logf("bootstrap projection for %s: %v", vmID, serr)
	}
	if r.Phase == PhaseActive && c.ControlAllowed(r.OwnerUserID) && vm.State == vmlib.StateRunning {
		if err := c.ensureTerminalLocked(ctx, r); err != nil {
			c.logf("terminal rebuild for %s: %v", vmID, err)
		}
	}
}
