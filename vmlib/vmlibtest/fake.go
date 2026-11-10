// Package vmlibtest provides a fake in-memory vmlib.Backend. It exercises
// orchestration, transport and policy logic; it is NOT evidence that a real VM
// boots, gets a TAP device, or leaves a launcher receipt.
package vmlibtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Backend is a fake host backend with admission caps and unique guest IPs.
type Backend struct {
	mu       sync.Mutex
	caps     vmlib.Capabilities
	images   map[string]vmlib.Image
	vms      map[string]*fakeVM
	ips      map[int]bool
	exitHold map[string]chan struct{}

	// AutoExit makes RequestShutdown produce an immediate clean exit receipt.
	AutoExit bool
	// UncleanExit makes simulated exits unclean (non-zero status).
	UncleanExit bool
	// StartErr, when set, is returned by Start before the VM runs.
	StartErr error
	// StartUncertainErr, when set, makes Start run the VM then return an error.
	StartUncertainErr error
	// GuestDNS is advertised in guest network metadata (default 1.1.1.1; nil = none).
	GuestDNS []string
	// OnInspect runs inside Inspect AFTER the state was read and the lock was
	// released, so a test can make a read return stale data.
	OnInspect func(id string)
	// OnStart runs inside Start (before returning); useful for races.
	OnStart func(vmID string)

	Prepared, Started, Published, Released int
}

type fakeVM struct {
	host  vmlib.HostVM
	slot  int
	exit  chan struct{}
	clean bool
	done  bool
}

// New returns a backend with max VMs, profile "small" and the given base images.
func New(maxVMs int, baseImages ...string) *Backend {
	b := &Backend{
		caps: vmlib.Capabilities{
			MaxVMs:      maxVMs,
			Profiles:    []vmlib.Profile{{Name: "small", VCPU: 1, MemMiB: 512, DiskMiB: 2048}},
			Bastion:     &vmlib.Bastion{Host: "bastion.example.test", Port: 22, User: "jump"},
			SSHUser:     "root",
			SSHAuthMode: vmlib.AuthImagePreinstalled,
		},
		images:   map[string]vmlib.Image{},
		vms:      map[string]*fakeVM{},
		ips:      map[int]bool{},
		exitHold: map[string]chan struct{}{},
		AutoExit: true,
		GuestDNS: []string{"1.1.1.1"},
	}
	for _, id := range baseImages {
		b.images[id] = vmlib.Image{ID: id, SizeBytes: 1 << 20, SHA256: "base-" + id, Base: true}
	}
	return b
}

func (b *Backend) SetCapabilities(c vmlib.Capabilities) { b.mu.Lock(); b.caps = c; b.mu.Unlock() }

func (b *Backend) Capabilities(context.Context) (vmlib.Capabilities, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.caps, nil
}

func (b *Backend) ListImages(context.Context) ([]vmlib.Image, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []vmlib.Image
	for _, i := range b.images {
		out = append(out, i)
	}
	return out, nil
}

func (b *Backend) GetImage(_ context.Context, id string) (vmlib.Image, error) {
	if err := vmlib.ValidateID("image", id); err != nil {
		return vmlib.Image{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	i, ok := b.images[id]
	if !ok {
		return vmlib.Image{}, vmlib.ErrNotFound
	}
	return i, nil
}

func (b *Backend) Prepare(_ context.Context, r vmlib.PrepareRequest) (vmlib.HostVM, error) {
	if err := vmlib.ValidateID("vm", r.VMID); err != nil {
		return vmlib.HostVM{}, err
	}
	if err := vmlib.ValidateID("image", r.ImageID); err != nil {
		return vmlib.HostVM{}, err
	}
	if err := vmlib.ValidatePublicKey(r.SSHPublicKey); err != nil {
		return vmlib.HostVM{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.vms[r.VMID]; ok {
		if v.host.ImageID == r.ImageID && v.host.Profile == r.Profile {
			return v.host, nil
		}
		return vmlib.HostVM{}, vmlib.ErrAlreadyExists
	}
	if _, ok := b.images[r.ImageID]; !ok {
		return vmlib.HostVM{}, vmlib.ErrNotFound
	}
	known := false
	for _, p := range b.caps.Profiles {
		known = known || p.Name == r.Profile
	}
	if !known {
		return vmlib.HostVM{}, fmt.Errorf("%w: unknown profile %q", vmlib.ErrInvalidArgument, r.Profile)
	}
	if len(b.vms) >= b.caps.MaxVMs {
		return vmlib.HostVM{}, vmlib.ErrResourceExhausted
	}
	slot := 0
	for b.ips[slot] {
		slot++
	}
	b.ips[slot] = true
	b.Prepared++
	v := &fakeVM{slot: slot, exit: make(chan struct{})}
	v.host = vmlib.HostVM{ID: r.VMID, ImageID: r.ImageID, Profile: r.Profile, State: vmlib.HostPrepared,
		GuestIP: fmt.Sprintf("172.16.%d.2", slot+1), PublicKeyProvisioned: r.SSHPublicKey != "",
		Guest: &vmlib.GuestNetwork{Address: fmt.Sprintf("172.16.%d.2/30", slot+1), Gateway: fmt.Sprintf("172.16.%d.1", slot+1),
			DNS: append([]string(nil), b.GuestDNS...), Configuration: "mac-derived"}}
	b.vms[r.VMID] = v
	return v.host, nil
}

func (b *Backend) get(id string) (*fakeVM, error) {
	v, ok := b.vms[id]
	if !ok {
		return nil, vmlib.ErrNotFound
	}
	return v, nil
}

func (b *Backend) Start(_ context.Context, id string) (vmlib.HostVM, error) {
	b.mu.Lock()
	v, err := b.get(id)
	if err != nil {
		b.mu.Unlock()
		return vmlib.HostVM{}, err
	}
	if b.StartErr != nil {
		b.mu.Unlock()
		return vmlib.HostVM{}, b.StartErr
	}
	if v.host.State == vmlib.HostPrepared {
		v.host.State = vmlib.HostRunning
		b.Started++
	}
	h, hook, uncertain := v.host, b.OnStart, b.StartUncertainErr
	b.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if uncertain != nil {
		return vmlib.HostVM{}, uncertain
	}
	return h, nil
}

func (b *Backend) Inspect(_ context.Context, id string) (vmlib.HostVM, error) {
	b.mu.Lock()
	v, err := b.get(id)
	if err != nil {
		b.mu.Unlock()
		return vmlib.HostVM{}, err
	}
	h, hook := v.host, b.OnInspect
	b.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	return h, nil
}

func (b *Backend) List(context.Context) ([]vmlib.HostVM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []vmlib.HostVM
	for _, v := range b.vms {
		out = append(out, v.host)
	}
	return out, nil
}

func (b *Backend) exitLocked(v *fakeVM, clean bool) {
	if v.done {
		return
	}
	v.done, v.clean = true, clean
	v.host.State = vmlib.HostExited
	close(v.exit)
}

// ExitVM simulates the VMM's exit receipt appearing.
func (b *Backend) ExitVM(id string, clean bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v, ok := b.vms[id]; ok {
		b.exitLocked(v, clean)
	}
}

func (b *Backend) RequestShutdown(_ context.Context, id string) (vmlib.HostVM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.get(id)
	if err != nil {
		return vmlib.HostVM{}, err
	}
	if v.host.State == vmlib.HostRunning {
		v.host.State = vmlib.HostExiting
		if b.AutoExit {
			b.exitLocked(v, !b.UncleanExit)
		}
	}
	return v.host, nil
}

func (b *Backend) WaitExit(ctx context.Context, id string) (vmlib.ExitReceipt, error) {
	b.mu.Lock()
	v, err := b.get(id)
	b.mu.Unlock()
	if err != nil {
		return vmlib.ExitReceipt{}, err
	}
	select {
	case <-v.exit:
		b.mu.Lock()
		defer b.mu.Unlock()
		return vmlib.ExitReceipt{VMID: id, Clean: v.clean}, nil
	case <-ctx.Done():
		return vmlib.ExitReceipt{}, ctx.Err()
	}
}

func (b *Backend) Publish(_ context.Context, id, newID string) (vmlib.Image, error) {
	if err := vmlib.ValidateID("image", newID); err != nil {
		return vmlib.Image{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	v, err := b.get(id)
	if err != nil {
		return vmlib.Image{}, err
	}
	if !v.done || !v.clean {
		return vmlib.Image{}, vmlib.ErrNotStopped
	}
	if _, ok := b.images[newID]; ok {
		return vmlib.Image{}, vmlib.ErrAlreadyExists
	}
	img := vmlib.Image{ID: newID, SizeBytes: 1 << 20, SHA256: "from-" + id}
	b.images[newID] = img
	b.Published++
	return img, nil
}

func (b *Backend) Release(_ context.Context, id string) (vmlib.ReleaseResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.vms[id]
	if !ok {
		return vmlib.ReleaseResult{Released: true, Detail: "already released"}, nil
	}
	if v.host.State != vmlib.HostPrepared && !v.done {
		v.host.Retained, v.host.RetainedReason = true, "exit receipt not confirmed"
		return vmlib.ReleaseResult{Released: false, Detail: v.host.RetainedReason}, nil
	}
	delete(b.ips, v.slot)
	delete(b.vms, id)
	b.Released++
	return vmlib.ReleaseResult{Released: true}, nil
}

// Count returns the number of VMs currently holding leases.
func (b *Backend) Count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.vms) }
