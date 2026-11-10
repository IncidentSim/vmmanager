package vmbot

// The control domain shares the existing Store, lifetime lock and DBOS queue.
// Domain intent is never allocated inside a replayable workflow.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

var ErrControlConflict = errors.New("conflicting request or resource state")
var ErrControlForbidden = errors.New("caller is not allowed")

type ControlResources struct {
	VCPU      uint32 `json:"vcpu"`
	MemoryMiB uint32 `json:"memory_mib"`
}
type ControlPolicy struct {
	AllowedUsers          []int64
	SharedImages          []string
	MaxVMs, MaxVMsPerUser int
}
type ControlRequest struct {
	Kind         string            `json:"kind"`
	VMID         string            `json:"vm_id,omitempty"`
	ImageID      string            `json:"image_id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Resources    *ControlResources `json:"resources,omitempty"`
	SSHPublicKey string            `json:"ssh_public_key,omitempty"`
}
type ControlOperation struct {
	ID          string `json:"id"`
	OwnerUserID int64  `json:"owner_user_id"`
	Key         string `json:"key"`
	Digest      string `json:"digest"`
	Kind        string `json:"kind"`
	VMID        string `json:"vm_id,omitempty"`
	ImageID     string `json:"image_id,omitempty"`
	Status      string `json:"status"`
	ErrorCode   string `json:"error_code,omitempty"`
	Attempt     uint64 `json:"attempt,omitempty"`
	Repair      bool   `json:"repair,omitempty"`
}
type ControlImage struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnerUserID int64  `json:"owner_user_id,omitempty"`
	// Shared is stored false for every creator-owned image (validateControlState
	// enforces it). In projections it is true for every catalog entry: the
	// registry is common, so Shared means "visible to all admitted users", while
	// OwnerUserID keeps the creator authority that only CanDelete exposes.
	Shared bool   `json:"shared"`
	Phase  string `json:"phase,omitempty"`
	// CanDelete is a projection-only permission (creator and not a configured
	// system image). It never means "currently unreferenced"; delete may still 409.
	CanDelete bool `json:"can_delete,omitempty"`
}
type ControlVMView struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	ImageID   string            `json:"image_id"`
	State     string            `json:"state"`
	Phase     string            `json:"phase"`
	Resources *ControlResources `json:"resources"`
}

func validateControlState(s State) error {
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, v := range s.VMs {
		if v.OwnerUserID <= 0 {
			return errors.New("control VM requires positive immutable owner")
		}
	}
	for _, o := range s.Operations {
		k := fmt.Sprintf("%d:%s", o.OwnerUserID, o.Key)
		if o.OwnerUserID <= 0 || vmlib.ValidateID("operation", o.ID) != nil || ids[o.ID] || keys[k] || len(o.Digest) != 64 {
			return errors.New("invalid durable operation")
		}
		switch o.Status {
		case "pending", "running", "succeeded", "failed":
		default:
			return errors.New("invalid operation status")
		}
		ids[o.ID], keys[k] = true, true
	}
	ids = map[string]bool{}
	for _, i := range s.Images {
		if i.OwnerUserID <= 0 || i.Shared || vmlib.ValidateID("image", i.ID) != nil || ValidateLabel(i.Name) != nil || ids[i.ID] {
			return errors.New("invalid private image")
		}
		ids[i.ID] = true
	}
	return nil
}

// OpenControlStore explicitly adopts legacy state only with a configured owner.
// OpenStore's existing v1 validation/backup happens first; exact v2 bytes are
// backed up before ownership is written. No first-user or owner-zero fallback.
func OpenControlStore(dir string, legacyOwner int64) (*Store, error) {
	s, err := openStore(dir, true, legacyOwner)
	if err != nil {
		return nil, err
	}
	if s.Snapshot().Control {
		return s, nil
	}
	if len(s.Snapshot().VMs) > 0 && legacyOwner <= 0 {
		s.Close()
		return nil, errors.New("legacy_owner_user_id required for nonempty legacy state")
	}
	raw, readErr := os.ReadFile(dir + "/state.json")
	if readErr == nil {
		if _, err = s.preserveBackup(raw); err != nil {
			s.Close()
			return nil, err
		}
	}
	err = s.Update(func(st *State) {
		st.Control = true
		for n := range st.VMs {
			st.VMs[n].OwnerUserID = legacyOwner
		}
		// Same write as the ownership adoption: a legacy deletion intent keeps
		// an owned control operation, so it survives legacy workflow retirement.
		synthesizeLegacyDeletes(st)
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// synthesizeLegacyDeletes gives every deleting VM that has no control delete
// operation one stable, owner-scoped, pending delete operation. The ID and key
// derive only from the immutable VM ID and delete Seq, so repeating this (a
// restart, or repair in RecoverControl) never adds a second operation. Legacy
// workflows are never resumed; this is the only handoff. A VM that already has
// any delete operation (even a failed one) is left to its explicit retry.
func synthesizeLegacyDeletes(s *State) {
	for _, v := range s.VMs {
		if v.Phase != PhaseDeleting || v.OwnerUserID <= 0 {
			continue
		}
		has := false
		for _, o := range s.Operations {
			has = has || (o.Kind == "delete" && o.VMID == v.ID)
		}
		if has {
			continue
		}
		key := fmt.Sprintf("legacy-delete:%s:%d", v.ID, v.Seq)
		sum := sha256.Sum256([]byte(key))
		digest := hex.EncodeToString(sum[:])
		s.Operations = append(s.Operations, ControlOperation{ID: "op-ld-" + digest[:24], OwnerUserID: v.OwnerUserID, Key: key, Digest: digest, Kind: "delete", VMID: v.ID, Status: "pending"})
	}
}

func (c *Controller) ConfigureControl(p ControlPolicy) { c.control = p }

// SetControlAuthorizer installs an additional, dynamic allow decision (for
// example durable username bindings that are still configured). It is consulted
// by every ownership/projection fence through ControlAllowed, so removing access
// also fences restart repair without touching any VM. Call before Start.
func (c *Controller) SetControlAuthorizer(f func(int64) bool) { c.authorizer = f }

// HasControlAuthorizer reports whether a dynamic authorizer is installed.
func (c *Controller) HasControlAuthorizer() bool { return c.authorizer != nil }
func (c *Controller) ControlAllowed(u int64) bool {
	if u <= 0 {
		return false
	}
	for _, a := range c.control.AllowedUsers {
		if a == u {
			return true
		}
	}
	return c.authorizer != nil && c.authorizer(u)
}
func (c *Controller) ControlPolicy() ControlPolicy { return c.control }
func (c *Controller) ControlVM(u int64, id string) (ControlVMView, error) {
	if !c.ControlAllowed(u) {
		return ControlVMView{}, ErrControlForbidden
	}
	r, ok := c.st.Snapshot().ByID(id)
	if !ok || r.OwnerUserID != u {
		return ControlVMView{}, ErrNoVM
	}
	return ControlVMView{r.ID, r.Label, r.ImageID, r.RemoteState, r.Phase, r.Resources}, nil
}
func (c *Controller) ControlVMs(u int64) []ControlVMView {
	out := []ControlVMView{}
	for _, v := range c.st.Snapshot().VMs {
		if v.OwnerUserID == u {
			if x, e := c.ControlVM(u, v.ID); e == nil {
				out = append(out, x)
			}
		}
	}
	return out
}
func (c *Controller) ControlImages(u int64) []ControlImage {
	out := []ControlImage{}
	if !c.ControlAllowed(u) {
		return out
	}
	protected := make(map[string]bool, len(c.control.SharedImages))
	state := c.st.Snapshot()
	for _, id := range c.control.SharedImages {
		if protected[id] {
			continue
		}
		protected[id] = true
		if i, ok := c.controlImage(state, u, id); ok {
			out = append(out, i)
		}
	}
	for _, i := range c.st.Snapshot().Images {
		if protected[i.ID] {
			continue
		}
		// The registry is common: every ready committed image is listed for every
		// admitted user. A deleting image stays listed only for its creator, with
		// its phase, so the creator can see and retry removal. Neither is bootable
		// while deleting, and publishing images are never listed.
		if i.Phase == "ready" || (i.Phase == "deleting" && i.OwnerUserID == u) {
			out = append(out, commonImage(i, u))
		}
	}
	return out
}

// commonImage projects a stored creator record into the common catalog.
func commonImage(i ControlImage, u int64) ControlImage {
	i.Shared = true
	i.CanDelete = i.OwnerUserID == u
	return i
}

// imageNameTaken reports a name reserved by a configured system image or any
// publishing/ready/deleting record of any creator. Pre-existing duplicates are
// never rewritten; this only gates new commits.
func (c *Controller) imageNameTaken(s State, name string) bool {
	for _, id := range c.control.SharedImages {
		if id == name {
			return true
		}
	}
	for _, i := range s.Images {
		if i.Name == name {
			return true
		}
	}
	return false
}

// ownedImage is the visibility lookup (system image, any ready image, or the
// caller's own deleting image). Never use it for boot eligibility; see controlImage.
func (c *Controller) ownedImage(s State, u int64, id string) (ControlImage, bool) {
	if i, ok := c.controlImage(s, u, id); ok {
		return i, true
	}
	for _, x := range s.Images {
		if x.ID == id && x.OwnerUserID == u && x.Phase == "deleting" {
			view := commonImage(x, u)
			for _, protected := range c.control.SharedImages {
				if protected == id {
					view.CanDelete = false
				}
			}
			return view, true
		}
	}
	return ControlImage{}, false
}
func (c *Controller) controlImage(s State, u int64, id string) (ControlImage, bool) {
	for _, x := range c.control.SharedImages {
		if x == id {
			for _, record := range s.Images {
				if record.ID == id && record.Phase != "ready" {
					return ControlImage{}, false
				}
			}
			return ControlImage{ID: id, Name: id, Shared: true, Phase: "ready"}, true
		}
	}
	for _, x := range s.Images {
		if x.ID == id && x.Phase == "ready" {
			return commonImage(x, u), true
		}
	}
	return ControlImage{}, false
}
func (c *Controller) ControlOperation(u int64, id string) (ControlOperation, error) {
	if !c.ControlAllowed(u) {
		return ControlOperation{}, ErrControlForbidden
	}
	for _, o := range c.st.Snapshot().Operations {
		if o.ID == id && o.OwnerUserID == u {
			return o, nil
		}
	}
	return ControlOperation{}, ErrNoVM
}
func (c *Controller) ControlTerminalURL(u int64, id string) (string, error) {
	if _, e := c.ControlVM(u, id); e != nil {
		return "", e
	}
	r, _ := c.st.Snapshot().ByID(id)
	if r.Phase != PhaseActive {
		return "", ErrNotRunning
	}
	return c.cfg.PublicOrigin + "/t/" + r.Token + "/", nil
}
func (c *Controller) ControlSSH(ctx context.Context, u int64, id string) (vmlib.SSHInfo, error) {
	if _, e := c.ControlVM(u, id); e != nil {
		return vmlib.SSHInfo{}, e
	}
	return c.lc.SSHInfo(ctx, id)
}

// AcceptControl atomically reserves identities, quota, image publication and
// destructive intent. An enqueue failure leaves pending intent for startup repair.
func (c *Controller) AcceptControl(u int64, key string, req ControlRequest) (ControlOperation, error) {
	if !c.ControlAllowed(u) {
		return ControlOperation{}, ErrControlForbidden
	}
	if len(key) < 1 || len(key) > 128 {
		return ControlOperation{}, vmlib.ErrInvalidArgument
	}
	for _, b := range []byte(key) {
		if b < 33 || b > 126 {
			return ControlOperation{}, vmlib.ErrInvalidArgument
		}
	}
	if req.Name != "" && ValidateLabel(req.Name) != nil {
		return ControlOperation{}, vmlib.ErrInvalidArgument
	}
	// Current backend supports image-preinstalled SSH authentication only.
	if req.SSHPublicKey != "" {
		return ControlOperation{}, vmlib.ErrInvalidArgument
	}
	if req.Resources != nil && (req.Resources.VCPU == 0 || req.Resources.MemoryMiB == 0) {
		return ControlOperation{}, vmlib.ErrInvalidArgument
	}
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	id, e := newVMID()
	if e != nil {
		return ControlOperation{}, e
	}
	oid := "op-" + strings.TrimPrefix(id, "bot-")
	tok, e := randToken()
	if e != nil {
		return ControlOperation{}, e
	}
	var out ControlOperation
	err := c.st.UpdateErr(func(s *State) error {
		if !s.Control {
			return ErrControlForbidden
		}
		for _, o := range s.Operations {
			if o.OwnerUserID == u && o.Key == key {
				if o.Digest != digest {
					return ErrControlConflict
				}
				out = o
				return nil
			}
		}
		out = ControlOperation{ID: oid, OwnerUserID: u, Key: key, Digest: digest, Kind: req.Kind, VMID: req.VMID, ImageID: req.ImageID, Status: "pending"}
		switch req.Kind {
		case "create":
			if _, ok := c.controlImage(*s, u, req.ImageID); !ok {
				return ErrNoVM
			}
			count := 0
			for _, v := range s.VMs {
				if v.OwnerUserID == u {
					count++
					if req.Name != "" && v.Label == req.Name {
						return ErrControlConflict
					}
				}
			}
			if len(s.VMs) >= c.control.MaxVMs || count >= c.control.MaxVMsPerUser {
				return ErrCapacity
			}
			s.Generation++
			name := req.Name
			if name == "" {
				name = fmt.Sprintf("vm-%d", s.Generation)
			}
			out.VMID = id
			s.VMs = append(s.VMs, VMRecord{ID: id, Token: tok, Phase: PhaseCreating, Generation: s.Generation, Label: name, OwnerUserID: u, ImageID: req.ImageID, Resources: req.Resources, SSHPublicKey: req.SSHPublicKey})
		case "stop", "delete", "commit", "terminal":
			r, ok := s.ByID(req.VMID)
			if !ok || r.OwnerUserID != u {
				return ErrNoVM
			}
			// No FIFO guarantee: don't accept a command whose prerequisite may be
			// queued later. Delete may supersede create; all grants recheck phase.
			if req.Kind != "delete" && !(req.Kind == "stop" && r.Phase == "stopping") && r.Phase != PhaseActive && r.Phase != "stopped" && r.Phase != "committed" {
				return ErrControlConflict
			}
			for _, o := range s.Operations {
				if o.VMID == r.ID && (o.Status == "pending" || o.Status == "running") && req.Kind != "delete" {
					return ErrControlConflict
				}
			}
			if req.Kind == "terminal" && r.Phase != PhaseActive {
				return ErrControlConflict
			}
			phase := r.Phase
			switch req.Kind {
			case "stop":
				phase = "stopping"
			case "delete":
				phase = PhaseDeleting
			case "commit":
				if req.Name == "" {
					return vmlib.ErrInvalidArgument
				}
				if c.imageNameTaken(*s, req.Name) {
					return ErrControlConflict
				}
				phase = "committing"
				out.ImageID = "img-" + strings.TrimPrefix(oid, "op-")
				s.Images = append(s.Images, ControlImage{ID: out.ImageID, Name: req.Name, OwnerUserID: u, Phase: "publishing"})
			}
			s.update(r.ID, func(v *VMRecord) { v.Phase = phase; v.Seq++ })
		case "delete_image":
			i, ok := c.ownedImage(*s, u, req.ImageID)
			if !ok {
				return ErrNoVM
			}
			// System base images are protected; ready common images are visible
			// to everyone but only their creator may delete them.
			if i.OwnerUserID == 0 {
				return ErrControlConflict
			}
			if !i.CanDelete {
				return ErrControlForbidden
			}
			// A fresh attempt on an already-deleting image is allowed only once
			// every earlier delete operation for it is terminal.
			for _, o := range s.Operations {
				if o.Kind == "delete_image" && o.ImageID == i.ID && (o.Status == "pending" || o.Status == "running") {
					return ErrControlConflict
				}
			}
			for _, v := range s.VMs {
				if v.ImageID == i.ID {
					return ErrControlConflict
				}
			}
			for n := range s.Images {
				if s.Images[n].ID == i.ID {
					s.Images[n].Phase = "deleting"
				}
			}
		default:
			return vmlib.ErrInvalidArgument
		}
		s.Operations = append(s.Operations, out)
		return nil
	})
	if err != nil {
		return ControlOperation{}, err
	}
	if out.Kind == "stop" || out.Kind == "commit" || out.Kind == "delete" {
		c.term.Revoke(out.VMID)
	}
	if out.Status == "pending" || out.Status == "running" {
		_ = c.submitControl(out)
	}
	return out, nil
}
func (c *Controller) submitControl(o ControlOperation) error {
	partition := o.VMID
	if partition == "" {
		partition = o.ImageID
	}
	id := "control-" + o.ID
	if o.Attempt > 0 {
		id = fmt.Sprintf("%s-%d", id, o.Attempt)
	}
	_, e := c.submit(OpInput{Kind: "control", VMID: partition, OperationID: o.ID}, id)
	return e
}
func (c *Controller) RecoverControl() error {
	var errs []error
	// Durable repair before any enqueue: deleting VMs without an operation (a
	// store migrated by an earlier build) get their stable delete operation.
	if e := c.st.UpdateErr(func(s *State) error {
		if s.Control {
			synthesizeLegacyDeletes(s)
		}
		return nil
	}); e != nil {
		errs = append(errs, e)
	}
	for _, o := range c.st.Snapshot().Operations {
		if o.Status == "pending" || o.Status == "running" {
			id := "control-" + o.ID
			if o.Attempt > 0 {
				id = fmt.Sprintf("%s-%d", id, o.Attempt)
			}
			if _, status := c.lookup(id); status != nil && !activeStatus(status.Status) && status.Status != dbos.WorkflowStatusSuccess {
				if e := c.st.Update(func(s *State) {
					for n := range s.Operations {
						if s.Operations[n].ID == o.ID {
							s.Operations[n].Attempt++
							s.Operations[n].Repair = true
							o = s.Operations[n]
						}
					}
				}); e != nil {
					errs = append(errs, e)
					continue
				}
			}
			errs = append(errs, c.submitControl(o))
		}
	}
	// Fresh startup projection work uses this same DBOS queue. An already
	// completed terminal workflow is not evidence of a live socket.
	for _, r := range c.st.Snapshot().VMs {
		if r.Phase == PhaseDeleting || r.Phase == "stopping" || r.Phase == "committing" {
			continue
		}
		id, e := newVMID()
		if e != nil {
			errs = append(errs, e)
			continue
		}
		sum := sha256.Sum256([]byte(id))
		o := ControlOperation{ID: "op-" + strings.TrimPrefix(id, "bot-"), OwnerUserID: r.OwnerUserID, Key: "recovery:" + id, Digest: hex.EncodeToString(sum[:]), Kind: "refresh", VMID: r.ID, Status: "pending"}
		e = c.st.Update(func(s *State) { s.Operations = append(s.Operations, o) })
		if e == nil {
			e = c.submitControl(o)
		}
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}
func (c *Controller) deletableImage(s State, op ControlOperation) bool {
	for _, id := range c.control.SharedImages {
		if id == op.ImageID {
			return false
		}
	}
	found := false
	for _, i := range s.Images {
		if i.ID == op.ImageID && i.OwnerUserID == op.OwnerUserID && i.Phase == "deleting" {
			found = true
		}
	}
	for _, v := range s.VMs {
		if v.ImageID == op.ImageID {
			return false
		}
	}
	return found
}
func (c *Controller) controlFlow(ctx dbos.Context, in OpInput) (OpResult, error) {
	// Unlike legacy attach-terminal steps, a terminal projection must be started
	// in the live body on replay, not recovered from a step checkpoint.
	err := c.executeControl(ctx, in.OperationID)
	return OpResult{}, err
}
func (c *Controller) executeControl(ctx context.Context, id string) error {
	var op ControlOperation
	for _, o := range c.st.Snapshot().Operations {
		if o.ID == id {
			op = o
		}
	}
	if op.ID == "" || op.Status == "succeeded" || op.Status == "failed" {
		return nil
	}
	if e := c.st.Update(func(s *State) {
		for n := range s.Operations {
			if s.Operations[n].ID == id {
				s.Operations[n].Status = "running"
			}
		}
	}); e != nil {
		return e
	}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.OpTimeout)
	defer cancel()
	var effectErr error
	absentBarrier := false
	var vm vmlib.VM
	r, exists := c.st.Snapshot().ByID(op.VMID)
	// Owner is immutable in intent. Recovery may clean up a removed user's
	// resources, but must never issue a new terminal grant for that owner.
	if op.Kind != "delete_image" && (!exists || r.OwnerUserID != op.OwnerUserID) {
		effectErr = ErrNoVM
	} else {
		switch op.Kind {
		case "refresh":
			vm, effectErr = c.lc.Get(qctx, r.ID)
			if effectErr == nil && r.Phase == PhaseActive && vm.State == vmlib.StateRunning && c.ControlAllowed(r.OwnerUserID) {
				effectErr = c.ensureTerminalLocked(qctx, r)
			}
		case "create":
			if r.Phase != PhaseCreating {
				effectErr = ErrControlConflict
				break
			}
			vm, effectErr = c.lc.Get(qctx, r.ID)
			if errors.Is(effectErr, vmlib.ErrNotFound) {
				if op.Repair {
					res, e := c.lc.Delete(qctx, r.ID)
					absentBarrier = e == nil && res.Deleted
					effectErr = ErrUncertain
					break
				}
				req := vmlib.CreateRequest{ID: r.ID, ImageID: r.ImageID, Profile: c.cfg.Profile, SSHPublicKey: r.SSHPublicKey}
				if r.Resources != nil {
					req.Profile = ""
					req.Resources = &vmlib.VMResources{VCPU: r.Resources.VCPU, MemMiB: r.Resources.MemoryMiB}
				}
				vm, effectErr = c.lc.Create(qctx, req)
			}
		case "stop":
			if r.Phase != "stopping" {
				effectErr = ErrControlConflict
				break
			}
			c.term.Revoke(r.ID)
			vm, effectErr = c.lc.Stop(qctx, r.ID)
		case "commit":
			if r.Phase != "committing" {
				effectErr = ErrControlConflict
				break
			}
			c.term.Revoke(r.ID)
			// Never adopt an unknown image on AlreadyExists: even a random collision
			// cannot confer ownership. Ambiguous publication retains the private intent.
			_, effectErr = c.lc.Stop(qctx, r.ID)
			if effectErr == nil {
				_, effectErr = c.lc.Commit(qctx, r.ID, op.ImageID)
			}
		case "delete":
			c.term.Revoke(r.ID)
			var res vmlib.DeleteResult
			res, effectErr = c.lc.Delete(qctx, r.ID)
			if effectErr == nil && !res.Deleted {
				effectErr = ErrUncertain
			}
		case "terminal":
			if !c.ControlAllowed(r.OwnerUserID) || r.Phase != PhaseActive {
				effectErr = ErrNotRunning
				break
			}
			effectErr = c.ensureTerminalLocked(qctx, r)
		case "delete_image":
			// Only a durably fenced, owned, unreferenced private image is ever
			// sent to the backend.
			if !c.deletableImage(c.st.Snapshot(), op) {
				effectErr = ErrControlConflict
				break
			}
			effectErr = c.lc.DeleteImage(qctx, op.ImageID)
		}
	}
	// A durable success is written only after the remote outcome is known. Errors
	// retain VM occupancy and terminal fences; retry uses a new explicit request.
	return c.st.Update(func(s *State) {
		for n := range s.Operations {
			if s.Operations[n].ID == id {
				s.Operations[n].Status = "succeeded"
				if effectErr != nil {
					s.Operations[n].Status = "failed"
					s.Operations[n].ErrorCode = "unavailable"
					if errors.Is(effectErr, vmlib.ErrAlreadyExists) || errors.Is(effectErr, ErrControlConflict) {
						s.Operations[n].ErrorCode = "conflict"
					}
					if errors.Is(effectErr, vmlib.ErrResourceExhausted) {
						s.Operations[n].ErrorCode = "capacity"
					}
					if errors.Is(effectErr, vmlib.ErrInvalidArgument) {
						s.Operations[n].ErrorCode = "invalid_argument"
					}
				}
			}
		}
		if effectErr != nil {
			if absentBarrier {
				s.remove(r.ID)
			}
			return
		}
		switch op.Kind {
		case "refresh":
			s.update(r.ID, func(v *VMRecord) {
				v.ImageID = vm.ImageID
				v.RemoteState = string(vm.State)
				if vm.Resources.VCPU != 0 {
					v.Resources = &ControlResources{vm.Resources.VCPU, vm.Resources.MemMiB}
				}
				if v.Phase == PhaseCreating {
					v.Phase = PhaseActive
				}
			})
		case "create":
			s.update(r.ID, func(v *VMRecord) {
				if v.Phase == PhaseCreating {
					v.Phase = PhaseActive
				}
				v.RemoteState = string(vm.State)
				if v.Resources == nil && vm.Resources.VCPU != 0 {
					v.Resources = &ControlResources{vm.Resources.VCPU, vm.Resources.MemMiB}
				}
			})
		case "stop":
			s.update(r.ID, func(v *VMRecord) {
				if v.Phase == "stopping" {
					v.Phase = "stopped"
				}
				v.RemoteState = string(vm.State)
			})
		case "commit":
			s.update(r.ID, func(v *VMRecord) {
				if v.Phase == "committing" {
					v.Phase = "committed"
				}
				v.RemoteState = "committed"
			})
			for n := range s.Images {
				if s.Images[n].ID == op.ImageID {
					s.Images[n].Phase = "ready"
				}
			}
		case "delete":
			s.remove(r.ID)
		case "delete_image":
			out := s.Images[:0]
			for _, i := range s.Images {
				if i.ID != op.ImageID {
					out = append(out, i)
				}
			}
			s.Images = out
		}
	})
}
