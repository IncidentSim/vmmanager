package controlapi

import (
	"context"
	"errors"
	"time"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/nextermaccess"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
)

// Browser statuses reported next to the raw SSH contract and terminal result.
const (
	BrowserReady       = "ready"
	BrowserUnsupported = "unsupported" // owner-supplied SSH key: raw SSH only
	BrowserPending     = "pending"     // provisioning deferred; retried by the API
)

// BrowserResult is the unified browser SSH link outcome. URL is a per-user
// session link; it must never be logged.
type BrowserResult struct {
	URL    string
	Status string
}

// BrowserLinker produces the unified browser SSH link for an owner's VM. It is
// the only terminal path when Nexterm is enabled (no ttyd).
type BrowserLinker interface {
	BrowserLink(ctx context.Context, owner int64, vmID string) (BrowserResult, error)
}

// NextermBrowser is the API-side owner of the durable Nexterm lifecycle. It
// adapts the in-process controller (ownership is proven by ControlVM, exactly
// like the HTTP routes) to nextermaccess.VMAPI, so no HTTP self-calls happen.
type NextermBrowser struct {
	mgr *nextermaccess.Manager
}

// NewNextermBrowser validates the admin credential file (no network) and builds
// the manager over the single owner of the state store.
func NewNextermBrowser(cfg nextermaccess.Config, nx *nextermaccess.Client, store *nextermaccess.Store, ctl *vmbot.Controller, keys KeyProvider) (*NextermBrowser, error) {
	m, e := nextermaccess.NewManager(cfg, nx, store, localVMAPI{ctl: ctl, keys: keys})
	if e != nil {
		return nil, e
	}
	if e := m.CheckCredentials(); e != nil {
		return nil, e
	}
	return &NextermBrowser{mgr: m}, nil
}

// Track durably schedules provisioning without any Nexterm network calls.
// Admission is already durable; startup also re-tracks the controller snapshot
// to repair a crash between admission and this secondary store write.
func (b *NextermBrowser) Track(owner int64, vmID string) error {
	return b.mgr.Track(owner, vmID)
}

// Run performs retry and delete/TTL cleanup until ctx ends.
func (b *NextermBrowser) Run(ctx context.Context) { b.mgr.Run(ctx) }

func (s *Server) trackBrowser(owner int64, vmID string) {
	if tracker, ok := s.opts.Browser.(interface{ Track(int64, string) error }); ok && vmID != "" {
		// The accepted operation must stay accepted even if the secondary store
		// is temporarily unavailable. A replay or API restart repairs the intent.
		_ = tracker.Track(owner, vmID)
	}
}

// Manager exposes the manager (tests).
func (b *NextermBrowser) Manager() *nextermaccess.Manager { return b.mgr }

func (b *NextermBrowser) BrowserLink(ctx context.Context, owner int64, vmID string) (BrowserResult, error) {
	l, e := b.mgr.BrowserLink(ctx, owner, vmID)
	switch {
	case e == nil:
		return BrowserResult{URL: l.Reveal(), Status: BrowserReady}, nil
	case errors.Is(e, nextermaccess.ErrUnsupported):
		return BrowserResult{Status: BrowserUnsupported}, nil
	case errors.Is(e, nextermaccess.ErrNotOwner):
		return BrowserResult{}, vmbot.ErrNoVM
	case errors.Is(e, nextermaccess.ErrVMInactive):
		return BrowserResult{}, vmbot.ErrNotRunning
	}
	// Pending and every transient Nexterm failure: no detail, no URL.
	return BrowserResult{Status: BrowserPending}, nil
}

// browser never fails its caller: any error degrades to a pending status
// without detail (ownership was already proven by the calling route).
func (s *Server) browser(ctx context.Context, u int64, id string) BrowserResult {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, e := s.opts.Browser.BrowserLink(ctx, u, id)
	if e != nil {
		return BrowserResult{Status: BrowserPending}
	}
	return res
}

type localVMAPI struct {
	ctl  *vmbot.Controller
	keys KeyProvider
}

func apiErr(e error) error {
	switch {
	case e == nil:
		return nil
	case errors.Is(e, vmbot.ErrNoVM):
		return &controlclient.APIError{Code: "not_found", Status: 404}
	case errors.Is(e, vmbot.ErrControlForbidden):
		return &controlclient.APIError{Code: "forbidden", Status: 403}
	case errors.Is(e, ErrKeyNotGenerated):
		return &controlclient.APIError{Code: "key_not_generated", Status: 409}
	}
	return errors.New("unavailable")
}

func toVM(v vmbot.ControlVMView) controlclient.VM {
	return controlclient.VM{ID: v.ID, Name: v.Name, ImageID: v.ImageID, State: v.State, Phase: v.Phase, Profile: v.Profile, TTLSeconds: v.TTLSeconds, ExpiresAt: v.ExpiresAt, OwnerError: v.OwnerError}
}

func (a localVMAPI) VM(_ context.Context, u int64, id string) (controlclient.VM, error) {
	v, e := a.ctl.ControlVM(u, id)
	return toVM(v), apiErr(e)
}

func (a localVMAPI) ListVMs(_ context.Context, u int64) ([]controlclient.VM, error) {
	if !a.ctl.ControlAllowed(u) {
		return nil, apiErr(vmbot.ErrControlForbidden)
	}
	out := []controlclient.VM{}
	for _, v := range a.ctl.ControlVMs(u) {
		out = append(out, toVM(v))
	}
	return out, nil
}

func (a localVMAPI) SSH(ctx context.Context, u int64, id string) (controlclient.SSH, error) {
	x, e := a.ctl.ControlSSH(ctx, u, id)
	if e != nil {
		return controlclient.SSH{}, apiErr(e)
	}
	return controlclient.SSH{Host: x.Host, Port: x.Port, User: x.User, HostKeyFingerprint: x.HostKeyFingerprint}, nil
}

func (a localVMAPI) SSHKey(ctx context.Context, u int64, id string) (controlclient.GeneratedKey, error) {
	if a.keys == nil {
		return controlclient.GeneratedKey{}, &controlclient.APIError{Code: "key_not_generated", Status: 409}
	}
	k, e := a.keys.GeneratedKey(ctx, u, id)
	if e != nil {
		return controlclient.GeneratedKey{}, apiErr(e)
	}
	return controlclient.GeneratedKey{VMID: id, PublicKey: k.PublicKey, PrivateKey: string(k.PrivateKeyPEM)}, nil
}
