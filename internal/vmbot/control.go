package vmbot

// The control domain shares the existing Store and lifetime lock; its execution
// belongs to the per-VM owner workflows (owner.go). This file is admission,
// projections, pre-owner handoff and startup recovery.
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
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

var ErrControlConflict = errors.New("conflicting request or resource state")
var ErrControlForbidden = errors.New("caller is not allowed")

// ErrExpired: the VM's lifetime has ended; ordinary commands are refused (the
// owner deletes the VM once its current child settles). Maps to code vm_expired.
var ErrExpired = errors.New("VM lifetime has expired")

// errCodeExpired is the stable operation error code for a refused command.
const errCodeExpired = "vm_expired"

// Default and maximum VM lifetime when ControllerConfig leaves them unset.
const (
	DefaultVMTTL = 3600 * time.Second
	MaxVMTTL     = 86400 * time.Second
)

// expired reports whether the record has a deadline that has been reached.
func expired(r VMRecord, now time.Time) bool {
	return r.ExpiresAt != nil && !now.Before(*r.ExpiresAt)
}

type ControlResources struct {
	VCPU          uint32 `json:"vcpu"`
	MemoryMiB     uint32 `json:"memory_mib"`
	CPULimitMilli uint32 `json:"cpu_limit_milli,omitempty"`
}
type ControlPolicy struct {
	AllowedUsers          []int64
	SharedImages          []string
	MaxVMs, MaxVMsPerUser int
}
type ControlRequest struct {
	Kind    string `json:"kind"`
	VMID    string `json:"vm_id,omitempty"`
	ImageID string `json:"image_id,omitempty"`
	Name    string `json:"name,omitempty"`
	// IntentVersion distinguishes current HTTP semantics from legacy raw DTO digests.
	IntentVersion int `json:"intent_version,omitempty"`
	// EffectiveProfile is admission-only; mutable defaults never enter intent.
	EffectiveProfile string `json:"-"`
	// Profile is the requested profile (empty preserves omission).
	Profile string `json:"profile,omitempty"`
	// TTLSeconds is omitted from the digest when unset, preserving old replay.
	TTLSeconds   int64  `json:"ttl_seconds,omitempty"`
	SSHPublicKey string `json:"ssh_public_key,omitempty"`
}
type ControlOperation struct {
	ID          string `json:"id"`
	AdminUserID int64  `json:"admin_user_id,omitempty"`
	OwnerUserID int64  `json:"owner_user_id"`
	Key         string `json:"key"`
	Digest      string `json:"digest"`
	Kind        string `json:"kind"`
	VMID        string `json:"vm_id,omitempty"`
	ImageID     string `json:"image_id,omitempty"`
	Status      string `json:"status"`
	ErrorCode   string `json:"error_code,omitempty"`
	// Attempt and Repair are written only by pre-owner builds; they stay in the
	// struct so strict state loading still accepts an upgraded file.
	Attempt uint64 `json:"attempt,omitempty"`
	Repair  bool   `json:"repair,omitempty"`
	// Exec is "owner" for every operation admitted by the owner model. An
	// operation without it is a pre-owner (legacy) record: see checkHandoff.
	Exec string `json:"exec,omitempty"`
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
	// Profile is the profile the VM was created with (absent for legacy VMs).
	Profile string `json:"profile,omitempty"`
	// OwnerError is true when the VM's owner workflow ended abnormally (ERROR,
	// cancelled, retries exhausted). The VM and its capacity stay retained;
	// commands are refused until an operator intervenes. Absent otherwise.
	OwnerError bool `json:"owner_error,omitempty"`
	// TTLSeconds/ExpiresAt are the persisted lifetime; absent for a legacy VM.
	TTLSeconds int64      `json:"ttl_seconds,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

func validateControlState(s State) error {
	if e := validateAdminBatches(s.AdminBatches); e != nil {
		return e
	}
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
	v := ControlVMView{ID: r.ID, Name: r.Label, ImageID: r.ImageID, State: r.RemoteState, Phase: r.Phase, Resources: r.Resources, Profile: r.Profile, TTLSeconds: r.TTLSeconds, ExpiresAt: r.ExpiresAt}
	if st, err := c.ownerState(r.ID); err == nil && st != "" && !ownerAlive(st) && st != dbos.WorkflowStatusSuccess {
		v.OwnerError = true
	}
	return v, nil
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
			// Read-only overlay: an unfinished operation whose owner has terminally
			// failed will never complete; report that instead of "pending" forever.
			// Nothing is written and nothing is retried here.
			if o.Exec == execOwner && o.VMID != "" && (o.Status == "pending" || o.Status == "running") {
				if st, err := c.ownerState(o.VMID); err == nil && st != "" && !ownerAlive(st) && st != dbos.WorkflowStatusSuccess {
					o.Status, o.ErrorCode = "failed", "unavailable"
				}
			}
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

// ControlSSH returns metadata even when readiness cannot be established. Ready
// is a fresh, host-side TCP observation, not authenticated SSH or client routing
// proof. This read never invokes the polling WaitSSHReady lifecycle operation.
func (c *Controller) ControlSSH(ctx context.Context, u int64, id string) (vmlib.SSHInfo, error) {
	if _, e := c.ControlVM(u, id); e != nil {
		return vmlib.SSHInfo{}, e
	}
	// Unlike detached workflow queries, HTTP reads honor caller cancellation.
	ctx, cancel := context.WithTimeout(ctx, c.cfg.QueryTimeout)
	defer cancel()
	info, err := c.lc.SSHInfo(ctx, id)
	if err != nil {
		return vmlib.SSHInfo{}, err
	}
	info.Ready = false // never forward a stale backend readiness bit
	active := func() bool {
		v, err := c.ControlVM(u, id)
		return err == nil && v.Phase == PhaseActive
	}
	// Public advertisement is not a hairpin/ingress probe target. The service
	// supplies a backend-derived per-VM private target; older lifecycle adapters
	// without a separate target retain their existing metadata behavior.
	probe := info
	if info.InternalTarget != nil {
		probe.Host, probe.Port = info.InternalTarget.Host, info.InternalTarget.Port
		probe.Bastion = nil
	}
	if !active() || probe.Host == "" || probe.Port <= 0 || probe.Port > 65535 {
		return info, nil
	}
	// Bound the entire observation (state checks and one TCP connection), rather
	// than polling until guest boot. The parent deadline may be shorter.
	probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
	defer probeCancel()
	vm, err := c.lc.Get(probeCtx, id)
	if err != nil || vm.State != vmlib.StateRunning || !active() {
		return info, nil
	}
	if vmlib.TCPProber(probeCtx, probe) != nil {
		return info, nil
	}
	// A stop/delete may have won while the network call was in flight. Do not
	// publish a successful probe from before a completed lifecycle transition.
	vm, err = c.lc.Get(probeCtx, id)
	info.Ready = err == nil && vm.State == vmlib.StateRunning && probeCtx.Err() == nil && active()
	return info, nil
}

// AcceptControl atomically reserves identities, quota, image publication and
// destructive intent. An enqueue failure leaves pending intent for startup repair.
func (c *Controller) AcceptControl(u int64, key string, req ControlRequest) (ControlOperation, error) {
	return c.acceptControl(u, key, req, 0)
}

// acceptControl receives admin identity only from the narrow, server-authorized batch path.
func (c *Controller) acceptControl(u int64, key string, req ControlRequest, admin int64) (ControlOperation, error) {
	if admin != 0 {
		if !c.AdminAllowed(admin) || req.Kind != "delete" {
			return ControlOperation{}, ErrControlForbidden
		}
	} else if !c.ControlAllowed(u) {
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
	if req.Kind != "create" && (req.Profile != "" || req.SSHPublicKey != "") {
		return ControlOperation{}, vmlib.ErrInvalidArgument
	}
	if req.SSHPublicKey != "" {
		if _, err := sshaccess.ParsePublicKey(req.SSHPublicKey); err != nil {
			return ControlOperation{}, vmlib.ErrInvalidArgument
		}
	}
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	digest := hex.EncodeToString(sum[:])
	if err := c.requireLiveOwner(u, key, req); err != nil {
		return ControlOperation{}, err
	}
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
		if req.TTLSeconds < 0 || (req.TTLSeconds > 0 && req.Kind != "create") || req.TTLSeconds > int64(c.cfg.MaxTTL/time.Second) {
			return vmlib.ErrInvalidArgument
		}
		out = ControlOperation{ID: oid, AdminUserID: admin, OwnerUserID: u, Key: key, Digest: digest, Kind: req.Kind, VMID: req.VMID, ImageID: req.ImageID, Status: "pending", Exec: execOwner}
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
			profile := req.Profile
			if profile == "" {
				profile = req.EffectiveProfile
			}
			var effective *ControlResources
			var diskMiB uint32
			if profile != "" {
				ctx, cancel := context.WithTimeout(context.Background(), c.cfg.OpTimeout)
				defer cancel()
				caps, err := c.lc.Capabilities(ctx)
				if err != nil {
					return err
				}
				prof, err := vmlib.ResolveProfile(caps.Profiles, profile)
				if err != nil {
					return err
				}
				diskMiB = prof.DiskMiB
				effective = &ControlResources{VCPU: prof.VCPU, MemoryMiB: prof.MemMiB, CPULimitMilli: prof.Resources().EffectiveCPULimitMilli()}
			}
			s.Generation++
			name := req.Name
			if name == "" {
				name = fmt.Sprintf("vm-%d", s.Generation)
			}
			ttl := req.TTLSeconds
			if ttl == 0 {
				ttl = int64(c.cfg.DefaultTTL / time.Second)
			}
			expires := c.now().Add(time.Duration(ttl) * time.Second)
			out.VMID = id
			s.VMs = append(s.VMs, VMRecord{ID: id, Token: tok, Phase: PhaseCreating, Generation: s.Generation, Label: name, OwnerUserID: u, ImageID: req.ImageID, Profile: profile, DiskMiB: diskMiB, Resources: effective, SSHPublicKey: req.SSHPublicKey, TTLSeconds: ttl, ExpiresAt: &expires})
		case "stop", "delete", "commit", "terminal":
			r, ok := s.ByID(req.VMID)
			if !ok || r.OwnerUserID != u {
				return ErrNoVM
			}
			if req.Kind != "delete" && expired(r, c.now()) {
				return ErrExpired
			}
			// The owner serializes execution; admission only bounds the backlog:
			// at most one unfinished command per VM, plus one Delete behind it.
			// The applied phase is never changed here.
			if req.Kind == "terminal" && r.Phase != PhaseActive {
				return ErrControlConflict
			}
			if req.Kind != "delete" && !(req.Kind == "stop" && r.Phase == "stopping") && r.Phase != PhaseActive && r.Phase != "stopped" && r.Phase != "committed" {
				return ErrControlConflict
			}
			for _, o := range s.Operations {
				if o.VMID == r.ID && (o.Status == "pending" || o.Status == "running") && (req.Kind != "delete" || o.Kind == "delete") {
					return ErrControlConflict
				}
			}
			if req.Kind == "commit" {
				if req.Name == "" {
					return vmlib.ErrInvalidArgument
				}
				if c.imageNameTaken(*s, req.Name) {
					return ErrControlConflict
				}
				out.ImageID = "img-" + strings.TrimPrefix(oid, "op-")
				s.Images = append(s.Images, ControlImage{ID: out.ImageID, Name: req.Name, OwnerUserID: u, Phase: "publishing"})
			}
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
	if out.Status == "pending" || out.Status == "running" {
		c.dispatch(&out)
	}
	return out, nil
}

// dispatch delivers an admitted operation: Create starts the VM's owner; other
// VM commands are sent to it; image deletion runs its own workflow. Delivery is
// idempotent by operation ID. A failure here leaves the durable intent pending
// (a duplicate request or the next startup re-delivers it); only a provably
// dead owner fails the operation immediately.
func (c *Controller) dispatch(o *ControlOperation) {
	var err error
	if o.Kind == "create" {
		err = c.startOwner(OwnerIn{VMID: o.VMID, CreateOpID: o.ID, CreateDigest: o.Digest})
	} else {
		err = c.deliver(*o)
	}
	if err == nil {
		return
	}
	c.logf("dispatch %s %s: %v", o.Kind, o.ID, err)
	if errors.Is(err, ErrOwnerUnavailable) {
		if e := c.st.Update(func(s *State) {
			for n := range s.Operations {
				if s.Operations[n].ID == o.ID && s.Operations[n].Status == "pending" {
					s.Operations[n].Status, s.Operations[n].ErrorCode = "failed", "unavailable"
					*o = s.Operations[n]
				}
			}
		}); e != nil {
			c.logf("dispatch %s: %v", o.ID, e)
		}
	}
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

// checkHandoff is the conservative pre-owner -> owner boundary. It reads only
// the store and runs before DBOS launches, so no owner ever exists beside an old
// executor. It rejects (with a diagnostic naming the VM and operation) any VM
// that an earlier build left in a transitional phase: its Create/Stop/Commit
// outcome is unknown to this build, and guessing could drop work, republish an
// image or create a replacement VM. Settled VMs (active, stopped, committed)
// and deleting VMs (Delete is idempotent and barrier-protected) are adopted.
// Operations the old build left unfinished and that this build cannot run are
// listed in the diagnostic only when they block adoption; see RecoverControl.
func checkHandoff(s State) error {
	var problems []string
	for _, v := range s.VMs {
		switch v.Phase {
		case PhaseActive, "stopped", "committed", PhaseDeleting:
			continue
		}
		owned := false
		var legacy []string
		for _, o := range s.Operations {
			if o.VMID != v.ID {
				continue
			}
			if o.Exec == execOwner {
				owned = true
			} else if o.Status == "pending" || o.Status == "running" {
				legacy = append(legacy, fmt.Sprintf("%s %s (%s)", o.Kind, o.ID, o.Status))
			}
		}
		if owned {
			continue
		}
		detail := "no recorded operation"
		if len(legacy) > 0 {
			detail = "unfinished " + strings.Join(legacy, ", ")
		}
		problems = append(problems, fmt.Sprintf("VM %s is in transitional phase %q with %s", v.ID, v.Phase, detail))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("cannot hand off pre-owner state: %s; finish or settle these with the previous build (or inspect the backend and repair the record deliberately) before starting this build", strings.Join(problems, "; "))
}

// RecoverControl runs after Start. It makes sure every stored VM has a living
// owner and re-delivers unfinished commands (delivery is idempotent by
// operation ID; the owner skips anything already finished). It never creates a
// replacement for an owner that ended abnormally: that is reported, the VM and
// its capacity stay retained, and an operator decides.
func (c *Controller) RecoverControl() error {
	var errs []error
	// A store migrated by an earlier build may hold a deleting VM without its
	// stable delete operation (explicit migration boundary, idempotent).
	if e := c.st.UpdateErr(func(s *State) error {
		if s.Control {
			synthesizeLegacyDeletes(s)
		}
		return nil
	}); e != nil {
		errs = append(errs, e)
	}
	snap := c.st.Snapshot()
	opsFor := func(vm string) (ops []ControlOperation) {
		for _, o := range c.st.Snapshot().Operations {
			if o.VMID == vm && (o.Status == "pending" || o.Status == "running") {
				ops = append(ops, o)
			}
		}
		return
	}
	unfinished := func(o ControlOperation) bool { return o.Status == "pending" || o.Status == "running" }
	for _, v := range snap.VMs {
		st, err := c.ownerState(v.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch {
		case ownerAlive(st):
		case st != "":
			errs = append(errs, fmt.Errorf("owner of VM %s is %s: VM retained, no replacement is started", v.ID, st))
			continue
		default:
			in := OwnerIn{VMID: v.ID}
			for _, o := range opsFor(v.ID) {
				if o.Kind == "create" && o.Exec == execOwner && v.Phase == PhaseCreating {
					in.CreateOpID, in.CreateDigest = o.ID, o.Digest
				}
			}
			if err := c.startOwner(in); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		for _, o := range opsFor(v.ID) {
			if o.Kind == "create" {
				continue // carried by the owner input
			}
			if o.Exec != execOwner {
				// A pre-owner operation. A delete is safely re-run by the owner;
				// terminal/refresh were rebuildable access projections; anything else
				// would already have been rejected by checkHandoff.
				if o.Kind != "delete" {
					c.markLegacyAbandoned(o.ID)
					continue
				}
				c.adoptLegacy(o.ID)
				o.Exec = execOwner
			}
			if err := c.deliver(o); err != nil {
				errs = append(errs, err)
			}
		}
	}
	// Operations whose VM no longer exists cannot run; image deletions are
	// re-driven by their own workflow.
	for _, o := range snap.Operations {
		if !unfinished(o) {
			continue
		}
		if o.Kind == "delete_image" {
			if o.Exec != execOwner {
				c.adoptLegacy(o.ID)
				o.Exec = execOwner
			}
			if err := c.deliver(o); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if _, ok := snap.ByID(o.VMID); !ok {
			c.markLegacyAbandoned(o.ID)
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) adoptLegacy(opID string) {
	_ = c.st.Update(func(s *State) {
		for n := range s.Operations {
			if s.Operations[n].ID == opID {
				s.Operations[n].Exec = execOwner
			}
		}
	})
}

// markLegacyAbandoned records, visibly, that an unfinished operation could not
// be carried over. The VM itself is untouched; the client may simply repeat it.
func (c *Controller) markLegacyAbandoned(opID string) {
	c.logf("operation %s was unfinished before the owner model and is marked failed (unavailable)", opID)
	_ = c.st.Update(func(s *State) {
		for n := range s.Operations {
			if s.Operations[n].ID == opID && (s.Operations[n].Status == "pending" || s.Operations[n].Status == "running") {
				s.Operations[n].Status, s.Operations[n].ErrorCode = "failed", "unavailable"
			}
		}
	})
}

// requireLiveOwner refuses a NEW command for a VM whose owner workflow ended
// abnormally or is gone: the operation would never run and must not be reported
// as accepted. A repeat of an existing request (same key) skips this and gets
// the recorded operation, so completed duplicates stay idempotent. No owner row
// at all is allowed: startup recovery has not created it yet.
func (c *Controller) requireLiveOwner(u int64, key string, req ControlRequest) error {
	switch req.Kind {
	case "stop", "delete", "commit", "terminal":
	default:
		return nil
	}
	for _, o := range c.st.Snapshot().Operations {
		if o.OwnerUserID == u && o.Key == key {
			return nil
		}
	}
	if r, ok := c.st.Snapshot().ByID(req.VMID); !ok || r.OwnerUserID != u {
		return nil // admission reports ErrNoVM
	}
	st, err := c.ownerState(req.VMID)
	if err != nil || st == "" || ownerAlive(st) {
		return nil
	}
	return ErrOwnerUnavailable
}
