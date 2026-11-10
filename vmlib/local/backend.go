//go:build linux

// Package local is the host backend: it owns every host resource (per-VM
// workspace disks, slot-derived TAP/IP/MAC/socket, receipt-confirmed exits and
// no-overwrite image publication). Each VM uses its own VMM OS process started
// through an externally installed launcher; this package never kills a process,
// never frees a disk on uncertain process state, and never accepts a path,
// command or address from a caller.
package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

const maxSlots = 64

var invocationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Launcher starts the externally installed per-slot VMM service. The slot is a
// bounded integer chosen by this backend; nothing else crosses this seam.
type Launcher interface {
	StartSlot(ctx context.Context, slot int) error
}

// VMMInfo is what a VMM's API reports about itself.
type VMMInfo struct{ ID, State string }

// MachineSpec contains only values derived by this backend from a slot, a
// profile and the server-owned workspace.
type MachineSpec struct {
	VCPU, MemMiB uint32
	Kernel       string
	BootArgs     string
	DiskJailPath string
	Tap, MAC     string
}

// VMM is the narrow control surface of one slot's Firecracker API socket.
type VMM interface {
	Info(ctx context.Context, slot int) (VMMInfo, error)
	Configure(ctx context.Context, slot int, spec MachineSpec) error
	InstanceStart(ctx context.Context, slot int) error
	CtrlAltDel(ctx context.Context, slot int) error
}

// Config is server policy. Clients never supply any of it.
type Config struct {
	WorkRoot   string // per-VM workspaces: <WorkRoot>/vms/<id>/{disk.ext4,meta.json}
	ImagesDir  string // image catalog: <id>.ext4
	RunDir     string // launcher runtime: <RunDir>/slot-<n>/<invocation>.exit.json
	MaxVMs     int
	Profiles   []vmlib.Profile
	SSHUser    string
	Bastion    *vmlib.Bastion
	Launcher   Launcher
	VMM        VMM
	MinFreeMiB uint64
	// CopyFile is optional (default: cp --reflink=auto --sparse=always).
	CopyFile func(src, dst string) error
	// ReadyTimeout bounds waiting for a started VMM's API (default 15s).
	ReadyTimeout time.Duration
}

const (
	statePreparing = "preparing"
	statePrepared  = vmlib.HostPrepared
	stateRunning   = vmlib.HostRunning
	stateExiting   = vmlib.HostExiting
	stateExited    = vmlib.HostExited
)

type meta struct {
	ID         string `json:"id"`
	ImageID    string `json:"image_id"`
	Profile    string `json:"profile"`
	Slot       int    `json:"slot"`
	State      string `json:"state"`
	Invocation string `json:"invocation,omitempty"`
	Retained   bool   `json:"retained,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type entry struct {
	mu sync.Mutex
	m  meta
}

// Backend implements vmlib.Backend on the local host.
type Backend struct {
	cfg Config

	mu      sync.Mutex
	entries map[string]*entry
	corrupt []string // unreadable workspaces: admission fails closed
}

var _ vmlib.Backend = (*Backend)(nil)

// New validates policy and reconciles existing host state, failing closed.
func New(cfg Config) (*Backend, error) {
	if cfg.MaxVMs < 1 || cfg.MaxVMs > maxSlots {
		return nil, fmt.Errorf("MaxVMs must be 1..%d", maxSlots)
	}
	if len(cfg.Profiles) == 0 || cfg.Launcher == nil || cfg.VMM == nil {
		return nil, errors.New("profiles, launcher and VMM controller are required")
	}
	for _, p := range []string{cfg.WorkRoot, cfg.ImagesDir, cfg.RunDir} {
		if !filepath.IsAbs(p) {
			return nil, errors.New("WorkRoot, ImagesDir and RunDir must be absolute")
		}
	}
	for _, p := range cfg.Profiles {
		if err := vmlib.ValidateID("profile", p.Name); err != nil {
			return nil, err
		}
	}
	if cfg.CopyFile == nil {
		cfg.CopyFile = copyFile
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 15 * time.Second
	}
	b := &Backend{cfg: cfg, entries: map[string]*entry{}}
	if err := os.MkdirAll(filepath.Join(cfg.WorkRoot, "vms"), 0o770); err != nil {
		return nil, err
	}
	return b, b.reconcile()
}

func (b *Backend) vmDir(id string) string    { return filepath.Join(b.cfg.WorkRoot, "vms", id) }
func (b *Backend) diskPath(id string) string { return filepath.Join(b.vmDir(id), "disk.ext4") }

// reconcile never frees anything. A VM that may still run keeps its lease.
func (b *Backend) reconcile() error {
	dirs, err := os.ReadDir(filepath.Join(b.cfg.WorkRoot, "vms"))
	if err != nil {
		return err
	}
	for _, d := range dirs {
		id := d.Name()
		data, err := os.ReadFile(filepath.Join(b.vmDir(id), "meta.json"))
		var m meta
		if err != nil || json.Unmarshal(data, &m) != nil || m.ID != id || vmlib.ValidateID("vm", id) != nil || m.Slot < 0 || m.Slot >= maxSlots {
			b.corrupt = append(b.corrupt, id)
			continue
		}
		e := &entry{m: m}
		switch m.State {
		case stateRunning, stateExiting:
			if _, done, _ := b.readReceipt(m); done {
				e.m.State, e.m.Retained, e.m.Reason = stateExited, false, ""
			} else {
				e.m.Retained, e.m.Reason = true, "restart: VMM exit not proven; disk retained"
			}
		}
		if e.m.Invocation != "" && e.m.State == statePrepared {
			e.m.Retained, e.m.Reason = true, "restart: configuration outcome unknown"
		}
		if e.m != m {
			if err := b.persist(&e.m); err != nil {
				return err
			}
		}
		b.entries[id] = e
	}
	return nil
}

func (b *Backend) persist(m *meta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	dir := b.vmDir(m.ID)
	f, err := os.CreateTemp(dir, ".meta-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "meta.json"))
}

func (b *Backend) profile(name string) (vmlib.Profile, bool) {
	for _, p := range b.cfg.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return vmlib.Profile{}, false
}

func slotIP(slot int) string  { return fmt.Sprintf("172.17.%d.2", slot) }
func slotMAC(slot int) string { return fmt.Sprintf("06:00:AC:11:%02X:02", slot) }
func slotTap(slot int) string { return fmt.Sprintf("tap-vm%d", slot) }

func (b *Backend) host(m meta) vmlib.HostVM {
	state := m.State
	if m.Retained {
		state = vmlib.HostRetained
	}
	if state == statePreparing {
		state = vmlib.HostPrepared
	}
	return vmlib.HostVM{ID: m.ID, ImageID: m.ImageID, Profile: m.Profile, State: state, GuestIP: slotIP(m.Slot),
		Retained: m.Retained, RetainedReason: m.Reason}
}

func (b *Backend) Capabilities(context.Context) (vmlib.Capabilities, error) {
	return vmlib.Capabilities{MaxVMs: b.cfg.MaxVMs, Profiles: append([]vmlib.Profile(nil), b.cfg.Profiles...),
		Bastion: b.cfg.Bastion, SSHUser: b.cfg.SSHUser, SSHAuthMode: vmlib.AuthImagePreinstalled}, nil
}

// ---- images ----------------------------------------------------------------

func (b *Backend) imagePath(id string) (string, os.FileInfo, error) {
	if err := vmlib.ValidateID("image", id); err != nil {
		return "", nil, err
	}
	p := filepath.Join(b.cfg.ImagesDir, id+".ext4")
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, fmt.Errorf("%w: image %s", vmlib.ErrNotFound, id)
	}
	if err != nil {
		return "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%w: image %s is not a regular file", vmlib.ErrInvalidArgument, id)
	}
	return p, info, nil
}

func (b *Backend) imageInfo(id string, info os.FileInfo, hash bool) vmlib.Image {
	_, err := os.Lstat(filepath.Join(b.cfg.ImagesDir, ".committed", id))
	img := vmlib.Image{ID: id, SizeBytes: uint64(info.Size()), Base: err != nil}
	if hash {
		img.SHA256, _ = fileSHA(filepath.Join(b.cfg.ImagesDir, id+".ext4"))
	}
	return img
}

func fileSHA(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (b *Backend) ListImages(context.Context) ([]vmlib.Image, error) {
	files, err := os.ReadDir(b.cfg.ImagesDir)
	if err != nil {
		return nil, err
	}
	var out []vmlib.Image
	for _, f := range files {
		name := f.Name()
		if filepath.Ext(name) != ".ext4" || name[0] == '.' {
			continue
		}
		id := name[:len(name)-len(".ext4")]
		if _, info, err := b.imagePath(id); err == nil {
			out = append(out, b.imageInfo(id, info, false))
		}
	}
	return out, nil
}

func (b *Backend) GetImage(_ context.Context, id string) (vmlib.Image, error) {
	_, info, err := b.imagePath(id)
	if err != nil {
		return vmlib.Image{}, err
	}
	return b.imageInfo(id, info, true), nil
}

func copyFile(src, dst string) error {
	cmd := exec.Command("cp", "--reflink=auto", "--sparse=always", "--", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy image: %w: %s", err, out)
	}
	return nil
}

// ---- admission ---------------------------------------------------------------

func (b *Backend) Prepare(ctx context.Context, req vmlib.PrepareRequest) (vmlib.HostVM, error) {
	for _, c := range [][2]string{{"vm", req.VMID}, {"image", req.ImageID}, {"profile", req.Profile}} {
		if err := vmlib.ValidateID(c[0], c[1]); err != nil {
			return vmlib.HostVM{}, err
		}
	}
	if req.SSHPublicKey != "" {
		return vmlib.HostVM{}, fmt.Errorf("%w: this backend does not provision SSH keys; use the image's preinstalled access", vmlib.ErrInvalidArgument)
	}
	prof, ok := b.profile(req.Profile)
	if !ok {
		return vmlib.HostVM{}, fmt.Errorf("%w: unknown profile %q", vmlib.ErrInvalidArgument, req.Profile)
	}
	src, info, err := b.imagePath(req.ImageID)
	if err != nil {
		return vmlib.HostVM{}, err
	}
	if uint64(info.Size()) > uint64(prof.DiskMiB)<<20 {
		return vmlib.HostVM{}, fmt.Errorf("%w: image larger than profile disk", vmlib.ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return vmlib.HostVM{}, err
	}

	b.mu.Lock()
	if len(b.corrupt) > 0 {
		b.mu.Unlock()
		return vmlib.HostVM{}, fmt.Errorf("host state unreadable for %d workspace(s); admission refused until an operator reviews them", len(b.corrupt))
	}
	if e, ok := b.entries[req.VMID]; ok {
		b.mu.Unlock()
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.m.ImageID == req.ImageID && e.m.Profile == req.Profile {
			return b.host(e.m), nil
		}
		return vmlib.HostVM{}, fmt.Errorf("%w: vm %s", vmlib.ErrAlreadyExists, req.VMID)
	}
	if len(b.entries) >= b.cfg.MaxVMs {
		b.mu.Unlock()
		return vmlib.HostVM{}, fmt.Errorf("%w: all %d VM slots are leased", vmlib.ErrResourceExhausted, b.cfg.MaxVMs)
	}
	used := map[int]bool{}
	for _, e := range b.entries {
		used[e.m.Slot] = true
	}
	slot := 0
	for used[slot] {
		slot++
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(b.cfg.WorkRoot, &st); err != nil {
		b.mu.Unlock()
		return vmlib.HostVM{}, err
	}
	if free := uint64(st.Bavail) * uint64(st.Bsize); free < uint64(info.Size())+b.cfg.MinFreeMiB<<20 {
		b.mu.Unlock()
		return vmlib.HostVM{}, fmt.Errorf("%w: insufficient workspace storage", vmlib.ErrResourceExhausted)
	}
	e := &entry{m: meta{ID: req.VMID, ImageID: req.ImageID, Profile: req.Profile, Slot: slot, State: statePreparing}}
	e.mu.Lock()
	b.entries[req.VMID] = e
	b.mu.Unlock()
	defer e.mu.Unlock()

	fail := func(err error) (vmlib.HostVM, error) {
		os.RemoveAll(b.vmDir(req.VMID))
		b.mu.Lock()
		delete(b.entries, req.VMID)
		b.mu.Unlock()
		return vmlib.HostVM{}, err
	}
	if err := os.Mkdir(b.vmDir(req.VMID), 0o770); err != nil {
		b.mu.Lock()
		delete(b.entries, req.VMID)
		b.mu.Unlock()
		return vmlib.HostVM{}, err
	}
	if err := os.Chmod(b.vmDir(req.VMID), 0o770); err != nil {
		return fail(err)
	}
	if err := b.persist(&e.m); err != nil {
		return fail(err)
	}
	if err := b.cfg.CopyFile(src, b.diskPath(req.VMID)); err != nil {
		return fail(err)
	}
	if err := os.Chmod(b.diskPath(req.VMID), 0o660); err != nil {
		return fail(err)
	}
	e.m.State = statePrepared
	if err := b.persist(&e.m); err != nil {
		return fail(err)
	}
	return b.host(e.m), nil
}

func (b *Backend) get(id string) (*entry, error) {
	if err := vmlib.ValidateID("vm", id); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[id]
	if !ok {
		return nil, fmt.Errorf("%w: vm %s", vmlib.ErrNotFound, id)
	}
	return e, nil
}

func (b *Backend) Inspect(_ context.Context, id string) (vmlib.HostVM, error) {
	e, err := b.get(id)
	if err != nil {
		return vmlib.HostVM{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return b.host(e.m), nil
}

func (b *Backend) List(context.Context) ([]vmlib.HostVM, error) {
	b.mu.Lock()
	es := make([]*entry, 0, len(b.entries))
	for _, e := range b.entries {
		es = append(es, e)
	}
	b.mu.Unlock()
	out := make([]vmlib.HostVM, 0, len(es))
	for _, e := range es {
		e.mu.Lock()
		out = append(out, b.host(e.m))
		e.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ---- start / shutdown / receipts --------------------------------------------

func (b *Backend) mustPersist(e *entry) error {
	if err := b.persist(&e.m); err != nil {
		return fmt.Errorf("persist VM state: %w; disk retained", err)
	}
	return nil
}

func (b *Backend) Start(ctx context.Context, id string) (vmlib.HostVM, error) {
	e, err := b.get(id)
	if err != nil {
		return vmlib.HostVM{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m.State != statePrepared || e.m.Retained || e.m.Invocation != "" {
		if e.m.State == stateRunning && !e.m.Retained {
			return b.host(e.m), nil // idempotent
		}
		return vmlib.HostVM{}, fmt.Errorf("%w: VM %s cannot be started from state %s", vmlib.ErrNotStopped, id, e.m.State)
	}
	prof, _ := b.profile(e.m.Profile)
	if err := b.cfg.Launcher.StartSlot(ctx, e.m.Slot); err != nil {
		return vmlib.HostVM{}, fmt.Errorf("launcher: %w", err)
	}
	info, err := b.waitVMM(ctx, e.m.Slot)
	if err != nil {
		return vmlib.HostVM{}, err
	}
	// From here a VMM may hold the disk: persist identity and retention first.
	e.m.Invocation, e.m.Retained, e.m.Reason = info.ID, true, "configuration in progress"
	if err := b.mustPersist(e); err != nil {
		return b.host(e.m), err
	}
	spec := MachineSpec{VCPU: prof.VCPU, MemMiB: prof.MemMiB, Kernel: "/kernel/vmlinux", BootArgs: "console=ttyS0 reboot=k panic=1 pci=off",
		DiskJailPath: "/work/vms/" + id + "/disk.ext4", Tap: slotTap(e.m.Slot), MAC: slotMAC(e.m.Slot)}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := b.cfg.VMM.Configure(cctx, e.m.Slot, spec); err != nil {
		e.m.Reason = "VMM configuration failed; disk retained: " + err.Error()
		_ = b.persist(&e.m)
		return b.host(e.m), fmt.Errorf("configure VMM: %w", err)
	}
	cur, err := b.cfg.VMM.Info(cctx, e.m.Slot)
	if err != nil || cur.ID != e.m.Invocation {
		e.m.Reason = "VMM identity changed during configuration; disk retained"
		_ = b.persist(&e.m)
		return b.host(e.m), errors.New(e.m.Reason)
	}
	e.m.State = stateRunning
	e.m.Reason = "InstanceStart response uncertain"
	_ = b.persist(&e.m)
	if err := b.cfg.VMM.InstanceStart(cctx, e.m.Slot); err != nil {
		return b.host(e.m), fmt.Errorf("InstanceStart response uncertain: %w", err)
	}
	e.m.Retained, e.m.Reason = false, ""
	if err := b.mustPersist(e); err != nil {
		return b.host(e.m), err
	}
	return b.host(e.m), nil
}

// waitVMM waits for a freshly launched VMM to be idle with a valid identity.
func (b *Backend) waitVMM(ctx context.Context, slot int) (VMMInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.ReadyTimeout)
	defer cancel()
	var last error
	for {
		info, err := b.cfg.VMM.Info(ctx, slot)
		switch {
		case err != nil:
			last = err
		case info.State != "Not started":
			return VMMInfo{}, fmt.Errorf("VMM slot %d is busy (%s); refusing to configure", slot, info.State)
		case !invocationPattern.MatchString(info.ID):
			return VMMInfo{}, errors.New("VMM reported an invalid invocation id; refusing to configure")
		default:
			return info, nil
		}
		select {
		case <-ctx.Done():
			return VMMInfo{}, fmt.Errorf("VMM slot %d not ready: %v: %w", slot, last, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type receiptFile struct {
	InvocationID string `json:"invocation_id"`
	Result       string `json:"result"`
	ExitCode     string `json:"exit_code"`
	ExitStatus   string `json:"exit_status"`
}

// readReceipt returns (clean, present, err). Only the ORIGINAL invocation's
// receipt counts; a responding socket or another invocation's file proves nothing.
func (b *Backend) readReceipt(m meta) (clean, present bool, err error) {
	if !invocationPattern.MatchString(m.Invocation) {
		return false, false, nil
	}
	path := filepath.Join(b.cfg.RunDir, fmt.Sprintf("slot-%d", m.Slot), m.Invocation+".exit.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	var r receiptFile
	if err := json.Unmarshal(data, &r); err != nil || r.InvocationID != m.Invocation {
		return false, false, errors.New("invalid exit receipt")
	}
	return r.Result == "success" && r.ExitCode == "exited" && r.ExitStatus == "0", true, nil
}

func (b *Backend) RequestShutdown(ctx context.Context, id string) (vmlib.HostVM, error) {
	e, err := b.get(id)
	if err != nil {
		return vmlib.HostVM{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.m.Invocation == "" || e.m.State == stateExited {
		return b.host(e.m), nil
	}
	if _, done, _ := b.readReceipt(e.m); done {
		return b.markExitedLocked(e), nil
	}
	info, err := b.cfg.VMM.Info(ctx, e.m.Slot)
	if err != nil {
		return b.host(e.m), fmt.Errorf("cannot confirm VMM identity: %w", err)
	}
	if info.ID != e.m.Invocation {
		// Never act on a replacement process; wait for the OLD receipt instead.
		return b.host(e.m), errors.New("VMM identity changed; refusing to signal a replacement process")
	}
	if err := b.cfg.VMM.CtrlAltDel(ctx, e.m.Slot); err != nil {
		// The VMM may exit before replying; only the receipt resolves this.
		return b.host(e.m), fmt.Errorf("request guest shutdown: %w", err)
	}
	if e.m.State == stateRunning {
		e.m.State = stateExiting
		_ = b.persist(&e.m)
	}
	return b.host(e.m), nil
}

func (b *Backend) markExitedLocked(e *entry) vmlib.HostVM {
	e.m.State, e.m.Retained, e.m.Reason = stateExited, false, ""
	_ = b.persist(&e.m)
	return b.host(e.m)
}

func (b *Backend) WaitExit(ctx context.Context, id string) (vmlib.ExitReceipt, error) {
	e, err := b.get(id)
	if err != nil {
		return vmlib.ExitReceipt{}, err
	}
	for {
		e.mu.Lock()
		m := e.m
		e.mu.Unlock()
		if m.Invocation == "" {
			return vmlib.ExitReceipt{}, fmt.Errorf("%w: VM %s was never started", vmlib.ErrNotStopped, id)
		}
		clean, done, rerr := b.readReceipt(m)
		if rerr != nil {
			return vmlib.ExitReceipt{}, rerr
		}
		if done {
			e.mu.Lock()
			b.markExitedLocked(e)
			e.mu.Unlock()
			detail := ""
			if !clean {
				detail = "VMM did not exit cleanly"
			}
			return vmlib.ExitReceipt{VMID: id, Clean: clean, Detail: detail}, nil
		}
		select {
		case <-ctx.Done():
			return vmlib.ExitReceipt{}, fmt.Errorf("waiting for exit receipt of %s: %w", id, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ---- publish / release --------------------------------------------------------

func (b *Backend) Publish(_ context.Context, id, newID string) (vmlib.Image, error) {
	if err := vmlib.ValidateID("image", newID); err != nil {
		return vmlib.Image{}, err
	}
	e, err := b.get(id)
	if err != nil {
		return vmlib.Image{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	clean, done, rerr := b.readReceipt(e.m)
	if rerr != nil {
		return vmlib.Image{}, rerr
	}
	if !done || !clean {
		return vmlib.Image{}, fmt.Errorf("%w: no clean exit receipt for %s", vmlib.ErrNotStopped, id)
	}
	dest := filepath.Join(b.cfg.ImagesDir, newID+".ext4")
	if _, err := os.Lstat(dest); err == nil {
		return vmlib.Image{}, fmt.Errorf("%w: image %s", vmlib.ErrAlreadyExists, newID)
	}
	tmp, err := os.CreateTemp(b.cfg.ImagesDir, ".publish-*.ext4")
	if err != nil {
		return vmlib.Image{}, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if err := b.cfg.CopyFile(b.diskPath(id), tmpPath); err != nil {
		return vmlib.Image{}, err
	}
	f, err := os.OpenFile(tmpPath, os.O_RDWR, 0)
	if err != nil {
		return vmlib.Image{}, err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return vmlib.Image{}, err
	}
	if err := os.Chmod(tmpPath, 0o444); err != nil {
		return vmlib.Image{}, err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, tmpPath, unix.AT_FDCWD, dest, unix.RENAME_NOREPLACE); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return vmlib.Image{}, fmt.Errorf("%w: image %s", vmlib.ErrAlreadyExists, newID)
		}
		return vmlib.Image{}, err
	}
	if err := os.MkdirAll(filepath.Join(b.cfg.ImagesDir, ".committed"), 0o755); err == nil {
		if mf, err := os.OpenFile(filepath.Join(b.cfg.ImagesDir, ".committed", newID), os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			mf.Close()
		}
	}
	info, err := os.Lstat(dest)
	if err != nil {
		return vmlib.Image{}, err
	}
	return b.imageInfo(newID, info, true), nil
}

func (b *Backend) Release(_ context.Context, id string) (vmlib.ReleaseResult, error) {
	if err := vmlib.ValidateID("vm", id); err != nil {
		return vmlib.ReleaseResult{}, err
	}
	b.mu.Lock()
	e, ok := b.entries[id]
	b.mu.Unlock()
	if !ok {
		return vmlib.ReleaseResult{Released: true, Detail: "already released"}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	neverStarted := e.m.Invocation == "" && (e.m.State == statePrepared || e.m.State == statePreparing) && !e.m.Retained
	if !neverStarted {
		if _, done, rerr := b.readReceipt(e.m); rerr != nil || !done {
			e.m.Retained = true
			if e.m.Reason == "" || e.m.Reason == "configuration in progress" {
				e.m.Reason = "exit receipt not confirmed"
			}
			_ = b.persist(&e.m)
			return vmlib.ReleaseResult{Released: false, Detail: e.m.Reason}, nil
		}
	}
	if err := os.RemoveAll(b.vmDir(id)); err != nil {
		return vmlib.ReleaseResult{}, err
	}
	b.mu.Lock()
	delete(b.entries, id)
	b.mu.Unlock()
	return vmlib.ReleaseResult{Released: true}, nil
}
