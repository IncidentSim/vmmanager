package vmlib

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// SSHProber reports nil once SSH is reachable for the described guest.
type SSHProber func(ctx context.Context, info SSHInfo) error

// Option configures a Service.
type Option func(*Service)

// WithSSHProber replaces the default readiness probe. The default only checks
// that the TCP port accepts a connection from THIS process; it does not
// authenticate SSH and cannot reach a host-private guest from a remote client.
// A remote client should supply a prober that goes through its bastion (for
// example `ssh -J ... true`) and treat the result as the readiness signal.
func WithSSHProber(p SSHProber) Option { return func(s *Service) { s.prober = p } }

// WithPollInterval sets the WaitSSHReady polling period.
func WithPollInterval(d time.Duration) Option { return func(s *Service) { s.poll = d } }

// TCPProber is the default probe: the port accepts a TCP connection.
func TCPProber(ctx context.Context, info SSHInfo) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(info.Host, fmt.Sprint(info.Port)))
	if err != nil {
		return err
	}
	return c.Close()
}

// Service is the single lifecycle facade implementation. It holds no host
// resources itself; everything lives behind Backend. It installs no signal
// handlers and never ties a VM's life to a call's context.
type Service struct {
	b      Backend
	prober SSHProber
	poll   time.Duration

	mu   sync.Mutex
	recs map[string]*record
	// tombs holds every (validated) ID for which Delete was ever attempted,
	// whether it later succeeded, failed or was cancelled. Entries never expire
	// for the lifetime of this Service: once Delete has started, that ID can never
	// be admitted by Create again, so a delayed, retried or successor Create
	// cannot resurrect it or make Delete act on a stale record. IDs are therefore
	// single-use per Service instance (one small map entry per Delete-d ID). The
	// set is in memory only: a Service restart forgets it.
	tombs map[string]struct{}
}

var _ Lifecycle = (*Service)(nil)

// record has two locks. op (context-aware) serializes lifecycle operations and
// may be held across slow backend calls. st protects every mutable field and is
// only ever held for short, non-blocking sections, so Get/List/SSHInfo never
// wait for a lifecycle operation.
type record struct {
	op chan struct{}

	st           sync.Mutex
	ver          uint64 // bumped on every state change; guards late application of reads
	vm           VM
	keyRef       string // immutable after publication
	exited       bool   // original exit receipt observed (or never started)
	clean        bool
	neverStarted bool
	deleted      bool
	creating     bool // Create has not settled: the backend may not know the VM yet
	// uncertainPrepare: Prepare failed ambiguously and the backend gave no proof
	// that the request settled. The identity is kept (retained) and is never
	// cleared merely because the backend reports NotFound.
	uncertainPrepare bool
}

func (r *record) lock(ctx context.Context) error {
	select {
	case r.op <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *record) unlock() { <-r.op }

// NewService builds the facade over any Backend (local, remote or fake).
func NewService(b Backend, opts ...Option) (*Service, error) {
	if b == nil {
		return nil, fmt.Errorf("%w: nil backend", ErrInvalidArgument)
	}
	s := &Service{b: b, prober: TCPProber, poll: time.Second, recs: map[string]*record{}, tombs: map[string]struct{}{}}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

func newRecord(vm VM) *record { return &record{op: make(chan struct{}, 1), vm: vm} }

func copyLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (r *record) snapshot() VM {
	r.st.Lock()
	defer r.st.Unlock()
	vm := r.vm
	vm.Labels = copyLabels(vm.Labels)
	return vm
}

func (r *record) isCreating() bool { r.st.Lock(); defer r.st.Unlock(); return r.creating }

func (r *record) isUncertainPrepare() bool { r.st.Lock(); defer r.st.Unlock(); return r.uncertainPrepare }

func (r *record) isDeleted() bool { r.st.Lock(); defer r.st.Unlock(); return r.deleted }

func (r *record) exitInfo() (exited, clean, never bool) {
	r.st.Lock()
	defer r.st.Unlock()
	return r.exited, r.clean, r.neverStarted
}

// mutate runs f under the state lock.
func (r *record) mutate(f func(r *record)) {
	r.st.Lock()
	defer r.st.Unlock()
	r.ver++
	f(r)
}

func (r *record) version() uint64 { r.st.Lock(); defer r.st.Unlock(); return r.ver }

// apply merges the backend's view unconditionally (used on paths that hold the
// operation lock, such as Create and rehydration).
func (r *record) apply(h HostVM) {
	r.st.Lock()
	defer r.st.Unlock()
	r.applyLocked(h)
}

// applyIfUnchanged merges a read only if nothing changed the record since the
// read began (ver). A read that was overtaken by a lifecycle operation is stale
// and is dropped; the caller reports the record's current snapshot instead.
func (r *record) applyIfUnchanged(h HostVM, ver uint64) bool {
	r.st.Lock()
	defer r.st.Unlock()
	if r.ver != ver {
		return false
	}
	r.applyLocked(h)
	return true
}

// applyLocked is monotonic: once the original exit is known (stopped, failed,
// committed) no later read may move the VM back to running/exiting/preparing.
// Retention is sticky until a successful delete.
func (r *record) applyLocked(h HostVM) {
	if r.exited {
		return
	}
	if h.Retained {
		r.vm.Retained = true
		if r.vm.RetainedReason == "" {
			r.vm.RetainedReason = h.RetainedReason
		}
	}
	before := r.vm.State
	switch h.State {
	case HostPrepared:
		if r.vm.State != StateFailed && r.vm.State != StateStopped {
			r.vm.State = StatePreparing
		}
	case HostRunning:
		if r.vm.State != StateStopping {
			r.vm.State = StateRunning
		}
	case HostExiting:
		r.vm.State = StateStopping
	case HostExited:
		if r.vm.State != StateCommitted && r.vm.State != StateFailed {
			r.vm.State = StateStopped
		}
	}
	if r.vm.State != before || h.Retained {
		r.ver++
	}
}

func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "vm-" + hex.EncodeToString(b[:]), nil
}

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// Create prepares and starts a VM from an image. ctx bounds this call only:
// once InstanceStart may have been issued the VM keeps running when ctx ends.
//
// On an ambiguous failure the returned VM carries the ID and Retained state so
// the caller can reconcile with Get/Stop/Delete. Callers that cannot receive a
// response (for example over a lost RPC) should supply CreateRequest.ID.
func (s *Service) Create(ctx context.Context, req CreateRequest) (VM, error) {
	if req.ID == "" {
		id, err := newID()
		if err != nil {
			return VM{}, err
		}
		req.ID = id
	}
	for _, c := range [][2]string{{"vm", req.ID}, {"image", req.ImageID}, {"profile", req.Profile}} {
		if err := ValidateID(c[0], c[1]); err != nil {
			return VM{}, err
		}
	}
	if err := ValidatePublicKey(req.SSHPublicKey); err != nil {
		return VM{}, err
	}
	if err := ctx.Err(); err != nil {
		return VM{}, err
	}
	rec := newRecord(VM{ID: req.ID, ImageID: req.ImageID, Profile: req.Profile, State: StatePreparing, Labels: copyLabels(req.Labels)})
	rec.keyRef = req.SSHKeyReference
	rec.creating = true
	s.mu.Lock()
	if _, ok := s.recs[req.ID]; ok {
		s.mu.Unlock()
		return VM{}, fmt.Errorf("%w: vm %s", ErrAlreadyExists, req.ID)
	}
	if _, dead := s.tombs[req.ID]; dead {
		s.mu.Unlock()
		return VM{}, fmt.Errorf("%w: vm %s was already deleted (IDs are single-use per Service)", ErrAlreadyExists, req.ID)
	}
	rec.op <- struct{}{} // held until creation finishes
	s.recs[req.ID] = rec
	s.mu.Unlock()
	defer rec.unlock()
	defer rec.mutate(func(r *record) { r.creating = false }) // runs before unlock: settled
	drop := func() {
		rec.mutate(func(r *record) { r.deleted = true })
		s.mu.Lock()
		delete(s.recs, req.ID)
		s.mu.Unlock()
	}
	retain := func(state VMState, reason string) {
		rec.mutate(func(r *record) {
			if state != "" {
				r.vm.State = state
			}
			r.vm.Retained, r.vm.RetainedReason = true, reason
		})
	}

	host, err := s.b.Prepare(ctx, PrepareRequest{VMID: req.ID, ImageID: req.ImageID, Profile: req.Profile, SSHPublicKey: req.SSHPublicKey})
	if err != nil {
		if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrResourceExhausted) || errors.Is(err, ErrAlreadyExists) {
			drop() // definitive refusal: nothing was prepared
			return VM{}, err
		}
		// Ambiguous transport failure: the Prepare request may still be running
		// on the backend, so neither Inspect NotFound nor a Release that reports
		// "already released" proves anything. Only a VM the backend actually
		// reports, still prepared and not started, can be released with proof.
		dctx, cancel := detached(ctx)
		defer cancel()
		if h, ierr := s.b.Inspect(dctx, req.ID); ierr == nil && h.State == HostPrepared && !h.Retained {
			if res, rerr := s.b.Release(dctx, req.ID); rerr == nil && res.Released {
				drop()
				return VM{}, err
			}
		}
		rec.mutate(func(r *record) { r.uncertainPrepare = true })
		retain(StateFailed, "prepare outcome uncertain; no backend proof it settled (operator recovery required)")
		return rec.snapshot(), err
	}
	rec.apply(host)
	if err := ctx.Err(); err != nil {
		dctx, cancel := detached(ctx)
		defer cancel()
		if res, rerr := s.b.Release(dctx, req.ID); rerr == nil && res.Released {
			drop()
			return VM{}, err
		}
		retain("", "cancelled after prepare; release not confirmed")
		return rec.snapshot(), err
	}
	if _, err := s.b.Start(ctx, req.ID); err != nil {
		dctx, cancel := detached(ctx)
		defer cancel()
		h, ierr := s.b.Inspect(dctx, req.ID)
		switch {
		case ierr == nil && h.State == HostPrepared && !h.Retained:
			// Provably nothing was launched: roll back and free the lease.
			if res, rerr := s.b.Release(dctx, req.ID); rerr == nil && res.Released {
				drop()
				return VM{}, err
			}
			retain(StateFailed, "start failed; release not confirmed")
		case ierr == nil:
			rec.apply(h) // the guest may be running; leave it for Get/Stop/Delete
		default:
			retain(StateFailed, "start outcome uncertain; VM identity unknown")
		}
		return rec.snapshot(), fmt.Errorf("start VM %s: %w", req.ID, err)
	}
	rec.mutate(func(r *record) { r.vm.State = StateRunning })
	return rec.snapshot(), nil
}

// lookup returns the record for id, rehydrating from the backend after a restart.
func (s *Service) lookup(ctx context.Context, id string) (*record, error) {
	if err := ValidateID("vm", id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	rec := s.recs[id]
	s.mu.Unlock()
	if rec != nil {
		return rec, nil
	}
	h, err := s.b.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	fresh := newRecord(VM{ID: id, ImageID: h.ImageID, Profile: h.Profile})
	fresh.apply(h)
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec := s.recs[id]; rec != nil {
		return rec, nil
	}
	s.recs[id] = fresh
	return fresh, nil
}

func (s *Service) refresh(ctx context.Context, rec *record) error {
	id := rec.snapshot().ID
	ver := rec.version() // the read is only applied if no operation overtakes it
	h, err := s.b.Inspect(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) && (rec.isCreating() || rec.snapshot().Retained) {
			// Create is still running, or its outcome is unconfirmed and the
			// identity is retained: the VM must stay visible even though the
			// backend does not know it (yet). NotFound would be a false proof;
			// only Delete may settle it.
			return nil
		}
		return err
	}
	rec.applyIfUnchanged(h, ver)
	return nil
}

func (s *Service) Get(ctx context.Context, id string) (VM, error) {
	rec, err := s.lookup(ctx, id)
	if err != nil {
		return VM{}, err
	}
	if rec.isDeleted() {
		return VM{}, ErrNotFound
	}
	if err := s.refresh(ctx, rec); err != nil {
		return VM{}, err
	}
	if rec.isDeleted() { // deleted while we were inspecting
		return VM{}, ErrNotFound
	}
	return rec.snapshot(), nil
}

func (s *Service) List(ctx context.Context) ([]VM, error) {
	hosts, err := s.b.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]VM, 0, len(hosts))
	for _, h := range hosts {
		vm, err := s.Get(ctx, h.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, vm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Service) ListImages(ctx context.Context) ([]Image, error) {
	imgs, err := s.b.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].ID < imgs[j].ID })
	return imgs, nil
}

func (s *Service) SSHInfo(ctx context.Context, id string) (SSHInfo, error) {
	rec, err := s.lookup(ctx, id)
	if err != nil {
		return SSHInfo{}, err
	}
	h, err := s.b.Inspect(ctx, id)
	if err != nil {
		return SSHInfo{}, err
	}
	caps, err := s.b.Capabilities(ctx)
	if err != nil {
		return SSHInfo{}, err
	}
	info := SSHInfo{Host: h.GuestIP, Port: 22, User: caps.SSHUser, AuthMode: caps.SSHAuthMode,
		PublicKeyProvisioned: h.PublicKeyProvisioned, HostKeyPolicy: HostKeyAcceptNewPerVM,
		HostKeyFingerprint: h.HostKeyFingerprint, Bastion: caps.Bastion, KeyReference: rec.keyRef,
		Guest: h.Guest}
	if info.AuthMode == "" {
		info.AuthMode = AuthImagePreinstalled
	}
	if h.PublicKeyProvisioned {
		info.AuthMode = AuthProvisionPublicKey
	}
	if h.HostKeyFingerprint != "" {
		info.HostKeyPolicy = HostKeyPinned
	}
	return info, nil
}

// WaitSSHReady polls the configured prober until it succeeds, ctx ends, or the
// VM stops. With the default prober "ready" means only "the TCP port accepted a
// connection"; see WaitReady and WithSSHProber.
func (s *Service) WaitSSHReady(ctx context.Context, id string) (SSHInfo, error) {
	return WaitReady(ctx, s, id, s.prober, s.poll)
}

// stopLocked requests graceful shutdown and waits for the receipt. rec.op held.
// The backend decides whether a shutdown request is safe (identity check); a
// VM that is retained/uncertain is still asked, never assumed stopped.
func (s *Service) stopLocked(ctx context.Context, rec *record) error {
	if exited, _, _ := rec.exitInfo(); exited {
		return s.exitError(rec)
	}
	id := rec.snapshot().ID
	h, err := s.b.Inspect(ctx, id)
	if err != nil {
		return err
	}
	if h.State == HostPrepared && !h.Retained {
		rec.mutate(func(r *record) {
			r.exited, r.clean, r.neverStarted = true, false, true
			r.vm.State = StateStopped
		})
		return nil
	}
	if h.State == HostRunning {
		rec.mutate(func(r *record) { r.vm.State = StateStopping })
		if _, err := s.b.RequestShutdown(ctx, id); err != nil {
			return fmt.Errorf("request shutdown: %w", err)
		}
	}
	rec.mutate(func(r *record) { r.vm.State = StateStopping })
	receipt, err := s.b.WaitExit(ctx, id)
	if err != nil {
		return fmt.Errorf("exit receipt for %s not confirmed: %w", id, err)
	}
	rec.mutate(func(r *record) {
		r.exited, r.clean = true, receipt.Clean
		r.vm.Retained, r.vm.RetainedReason = false, ""
		if !receipt.Clean {
			r.vm.State = StateFailed
			r.vm.RetainedReason = "unclean VMM exit: " + receipt.Detail
		} else if r.vm.State != StateCommitted {
			r.vm.State = StateStopped
		}
	})
	return s.exitError(rec)
}

func (s *Service) exitError(rec *record) error {
	_, clean, never := rec.exitInfo()
	if clean || never {
		return nil
	}
	return fmt.Errorf("%w: VM %s exited uncleanly", ErrNotStopped, rec.snapshot().ID)
}

// Stop gracefully stops a VM; it is idempotent and keeps the disk.
func (s *Service) Stop(ctx context.Context, id string) (VM, error) {
	rec, err := s.lookup(ctx, id)
	if err != nil {
		return VM{}, err
	}
	if err := rec.lock(ctx); err != nil {
		return VM{}, err
	}
	defer rec.unlock()
	if rec.isDeleted() {
		return VM{}, ErrNotFound
	}
	err = s.stopLocked(ctx, rec)
	return rec.snapshot(), err
}

// Commit stops the VM if needed, waits for its clean exit receipt and publishes
// its disk as a NEW image. It never overwrites an existing image and rejects a
// VM that never booted.
func (s *Service) Commit(ctx context.Context, id, newImageID string) (Image, error) {
	if err := ValidateID("image", newImageID); err != nil {
		return Image{}, err
	}
	rec, err := s.lookup(ctx, id)
	if err != nil {
		return Image{}, err
	}
	if err := rec.lock(ctx); err != nil {
		return Image{}, err
	}
	defer rec.unlock()
	if rec.isDeleted() {
		return Image{}, ErrNotFound
	}
	if err := s.stopLocked(ctx, rec); err != nil {
		return Image{}, err
	}
	if _, clean, never := rec.exitInfo(); never || !clean {
		return Image{}, fmt.Errorf("%w: VM %s has no clean boot/exit to commit", ErrNotStopped, id)
	}
	img, err := s.b.Publish(ctx, id, newImageID)
	if err != nil {
		return Image{}, err
	}
	rec.mutate(func(r *record) { r.vm.State = StateCommitted })
	return img, nil
}

// Delete stops the VM if needed and frees its disk and leases only when exit is
// proven. Otherwise it reports the disk as retained with ErrRetained.
func (s *Service) Delete(ctx context.Context, id string) (DeleteResult, error) {
	if err := ValidateID("vm", id); err != nil {
		return DeleteResult{}, err
	}
	// Delete INTENT spends the ID: from here no successor Create may be admitted,
	// even if this Delete later fails or is cancelled. Without this a Create that
	// settles and drops its record while we wait on its lock could be followed by
	// a same-ID replacement, and we would "delete" the stale record (ABA). The
	// Only syntactically valid IDs are recorded, but the map is NOT bounded by
	// the number of VMs: a (trusted) operator calling Delete on arbitrarily many
	// distinct valid IDs grows it by one small entry each for the Service's
	// lifetime. This is a deliberate trade-off: expiring entries would reopen the
	// resurrection window. s.mu is a leaf lock (never held across a record lock or
	// backend call), so this cannot deadlock.
	s.mu.Lock()
	s.tombs[id] = struct{}{}
	s.mu.Unlock()
	rec, err := s.lookup(ctx, id)
	if errors.Is(err, ErrNotFound) {
		// lookup found no record and the backend does not know the VM, but a
		// Create may have inserted its record since. Decide atomically with the
		// same lock Create uses to insert and to check tombstones.
		s.mu.Lock()
		rec = s.recs[id]
		if rec == nil {
			s.tombs[id] = struct{}{}
			s.mu.Unlock()
			return DeleteResult{Deleted: true, Detail: "already deleted"}, nil
		}
		s.mu.Unlock()
	} else if err != nil {
		return DeleteResult{}, err
	}
	// Barrier: waits for an in-flight Create (or any other operation) on this VM
	// to settle before anything is decided.
	if err := rec.lock(ctx); err != nil {
		return DeleteResult{}, err
	}
	defer rec.unlock()
	if rec.isDeleted() {
		s.mu.Lock()
		s.tombs[id] = struct{}{}
		s.mu.Unlock()
		return DeleteResult{Deleted: true, Detail: "already deleted"}, nil
	}
	retain := func(reason string) (DeleteResult, error) {
		rec.mutate(func(r *record) { r.vm.Retained, r.vm.RetainedReason = true, reason })
		return DeleteResult{Retained: true, Detail: reason}, fmt.Errorf("%w: %s", ErrRetained, reason)
	}
	if err := s.stopLocked(ctx, rec); err != nil {
		if errors.Is(err, ErrNotFound) && !rec.isUncertainPrepare() {
			// Creation has settled (we hold the operation lock) and the backend
			// has no such VM: nothing to free. Forget the record. (An ambiguous
			// Prepare is excluded: for it NotFound is not proof.)
			rec.mutate(func(r *record) { r.deleted = true; r.vm.State = StateDeleted })
			s.mu.Lock()
			delete(s.recs, id)
			s.tombs[id] = struct{}{}
			s.mu.Unlock()
			return DeleteResult{Deleted: true, Detail: "not present on backend"}, nil
		}
		if exited, _, _ := rec.exitInfo(); !exited {
			return retain(err.Error())
		}
	}
	res, err := s.b.Release(ctx, id)
	if err != nil {
		return retain("release failed: " + err.Error())
	}
	if !res.Released {
		return retain(res.Detail)
	}
	rec.mutate(func(r *record) { r.deleted = true; r.vm.State = StateDeleted })
	s.mu.Lock()
	delete(s.recs, id)
	s.tombs[id] = struct{}{}
	s.mu.Unlock()
	return DeleteResult{Deleted: true, Detail: res.Detail}, nil
}
