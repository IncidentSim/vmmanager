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
}

var _ Lifecycle = (*Service)(nil)

// record has two locks. op (context-aware) serializes lifecycle operations and
// may be held across slow backend calls. st protects every mutable field and is
// only ever held for short, non-blocking sections, so Get/List/SSHInfo never
// wait for a lifecycle operation.
type record struct {
	op chan struct{}

	st           sync.Mutex
	vm           VM
	keyRef       string // immutable after publication
	exited       bool   // original exit receipt observed (or never started)
	clean        bool
	neverStarted bool
	deleted      bool
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
	s := &Service{b: b, prober: TCPProber, poll: time.Second, recs: map[string]*record{}}
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

func (r *record) isDeleted() bool { r.st.Lock(); defer r.st.Unlock(); return r.deleted }

func (r *record) exitInfo() (exited, clean, never bool) {
	r.st.Lock()
	defer r.st.Unlock()
	return r.exited, r.clean, r.neverStarted
}

// mutate runs f under the state lock.
func (r *record) mutate(f func(r *record)) { r.st.Lock(); defer r.st.Unlock(); f(r) }

// apply merges the backend's view. Retention is sticky until a successful
// delete and is independent of the lifecycle state.
func (r *record) apply(h HostVM) {
	r.st.Lock()
	defer r.st.Unlock()
	if h.Retained {
		r.vm.Retained = true
		if r.vm.RetainedReason == "" {
			r.vm.RetainedReason = h.RetainedReason
		}
	}
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
	s.mu.Lock()
	if _, ok := s.recs[req.ID]; ok {
		s.mu.Unlock()
		return VM{}, fmt.Errorf("%w: vm %s", ErrAlreadyExists, req.ID)
	}
	rec.op <- struct{}{} // held until creation finishes
	s.recs[req.ID] = rec
	s.mu.Unlock()
	defer rec.unlock()
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
		drop()
		if !errors.Is(err, ErrInvalidArgument) && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrResourceExhausted) && !errors.Is(err, ErrAlreadyExists) {
			dctx, cancel := detached(ctx) // ambiguous transport failure: free only if never started
			_, _ = s.b.Release(dctx, req.ID)
			cancel()
		}
		return VM{}, err
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
	h, err := s.b.Inspect(ctx, id)
	if err != nil {
		return err
	}
	rec.apply(h)
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
// connection"; see WithSSHProber.
func (s *Service) WaitSSHReady(ctx context.Context, id string) (SSHInfo, error) {
	for {
		vm, err := s.Get(ctx, id)
		if err != nil {
			return SSHInfo{}, err
		}
		if vm.State != StateRunning {
			return SSHInfo{}, fmt.Errorf("%w: VM %s is %s", ErrNotStopped, id, vm.State)
		}
		info, err := s.SSHInfo(ctx, id)
		if err != nil {
			return SSHInfo{}, err
		}
		if perr := s.prober(ctx, info); perr == nil {
			info.Ready = true
			return info, nil
		}
		select {
		case <-ctx.Done():
			return SSHInfo{}, fmt.Errorf("waiting for SSH on %s: %w", id, ctx.Err())
		case <-time.After(s.poll):
		}
	}
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
	rec, err := s.lookup(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return DeleteResult{Deleted: true, Detail: "already deleted"}, nil
	}
	if err != nil {
		return DeleteResult{}, err
	}
	if err := rec.lock(ctx); err != nil {
		return DeleteResult{}, err
	}
	defer rec.unlock()
	if rec.isDeleted() {
		return DeleteResult{Deleted: true, Detail: "already deleted"}, nil
	}
	retain := func(reason string) (DeleteResult, error) {
		rec.mutate(func(r *record) { r.vm.Retained, r.vm.RetainedReason = true, reason })
		return DeleteResult{Retained: true, Detail: reason}, fmt.Errorf("%w: %s", ErrRetained, reason)
	}
	if err := s.stopLocked(ctx, rec); err != nil {
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
	s.mu.Unlock()
	return DeleteResult{Deleted: true, Detail: res.Detail}, nil
}
