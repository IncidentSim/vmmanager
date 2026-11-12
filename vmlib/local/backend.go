//go:build linux

// Package local is the host backend: it owns every host resource (per-VM
// workspace disks, slot-derived TAP/IP/MAC, receipt-confirmed exits and
// no-overwrite image publication). Each VM uses its own VMM OS process started
// through an externally installed launcher; this package never kills a process,
// never frees a disk on uncertain process state, and never accepts a path,
// command or address from a caller.
//
// Ownership: New takes exclusive, non-blocking flock locks on a lock file in
// each of WorkRoot, RunDir and ImagesDir (resolved through symlinks) and holds
// them until Close. Leases live in memory and in per-VM metadata, so a second
// owner of the same roots would double-allocate slots; the locks make that
// impossible. Lock files are never unlinked. Close releases the locks only: it
// never stops a VM or removes a workspace.
package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

const maxSlots = 64

var invocationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Launcher starts the externally installed per-slot VMM service. The slot and
// the VMM's CPU quota (millicores, the VM's persisted reservation) are chosen by
// this backend; nothing else crosses this seam. The launcher must make the unit
// run under exactly that CPU quota or fail. A returned error does NOT prove that
// nothing was started.
type Launcher interface {
	StartSlot(ctx context.Context, slot int, cpuMilli uint32) error
}

// VMMInfo is what a VMM's API reports about itself.
type VMMInfo struct{ ID, State string }

// MachineSpec contains only values derived by this backend from a slot, a
// profile and the server-owned workspace.
type MachineSpec struct {
	VCPU, MemMiB uint32
	// CPULimitMilli is informational here: the VMM API has no CPU-time knob; the
	// physical limit is the unit's CPUQuota applied through the Launcher.
	CPULimitMilli uint32
	Kernel        string
	BootArgs      string
	DiskJailPath  string
	Tap, MAC      string
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
	// CooperativeShutdown is an optional, bounded best-effort managed-guest
	// reboot request. It is not exit proof and is never used for legacy guests.
	CooperativeShutdown CooperativeShutdown
	// Access is an optional offline provisioner plus credential lifecycle hook.
	// It is called only on exclusively owned, unmounted disk copies.
	Access    OfflineAccess
	WorkRoot  string // per-VM workspaces: <WorkRoot>/vms/<id>/{disk.ext4,meta.json}
	ImagesDir string // image catalog: <id>.ext4
	RunDir    string // launcher runtime: <RunDir>/slot-<n>/<invocation>.exit.json
	MaxVMs    int
	Profiles  []vmlib.Profile
	SSHUser   string
	Bastion   *vmlib.Bastion
	PublicSSH *vmlib.PublicSSHConfig // operator-only; nil inherits PoolConf, or legacy routing without a pool
	Launcher  Launcher
	VMM       VMM

	// Aggregate admission (required). Together with MaxVMs they bound what can be
	// leased at once. Operators must keep them within the installed cgroup slice
	// budget (see deployment/resources.md); a slice limit alone is not admission.
	MaxTotalMemMiB uint32
	MaxTotalVCPU   uint32
	// MaxTotalCPULimitMilli bounds the SUM of the VMMs' CPU-time limits
	// (millicores). It must not exceed the parent slice's CPUQuota. 0 defaults to
	// MaxTotalVCPU*1000, i.e. the pre-cpu_limit meaning of the vCPU budget.
	MaxTotalCPULimitMilli uint32
	// DefaultProfile is advertised through Capabilities; "" picks "small" if it
	// is configured, else the first profile.
	DefaultProfile string
	// UnitMemLimitMiB is the installed per-instance MemoryMax; every profile must
	// satisfy MemMiB+VMMOverheadMiB <= UnitMemLimitMiB.
	UnitMemLimitMiB uint32
	VMMOverheadMiB  uint32 // default 64

	// GuestPrefix is the operator-chosen "A.B" of the guest pool: slot N uses
	// A.B.N.0/30 with the guest at .2 (and the MAC 06:00:A:B:N:02 that the CI
	// Ubuntu base derives its address from). Required: pick a range that collides
	// with nothing on the host (not Docker's 172.17/16).
	GuestPrefix string
	// PoolConf, when set, is the root-owned pool file shared with the network
	// script and helper; it must agree with GuestPrefix and cover MaxVMs.
	// Its publication settings are authoritative over PublicSSH.
	PoolConf string

	// GuestDNS and GuestConfiguration are advertised to clients as guest network
	// metadata. GuestConfiguration defaults to "mac-derived": the base image is
	// expected to derive its /30 address from the MAC (Firecracker CI Ubuntu rootfs).
	GuestNetwork       OfflineGuestNetwork // optional private-copy boot provisioning
	GuestDNS           []string
	GuestConfiguration string

	// TrustedOwnerUID is a TEST SEAM: the uid that must own the runtime tree
	// (default nil = root). Production code (vmapi) never sets it.
	TrustedOwnerUID *uint32
	// TrustedAncestorRoot is a TEST SEAM: ancestors ABOVE this directory are not
	// inspected (the directory itself still is). Default "" = inspect up to "/".
	TrustedAncestorRoot string

	// StorageMount, when set, must be a real mount point (device differs from its
	// parent) and WorkRoot and ImagesDir must live on that same device. Checked
	// before anything is written, so a missing mount cannot fall back to the root
	// filesystem.
	StorageMount string

	MinFreeMiB uint64
	// CopyFile is optional (default: cp --reflink=auto --sparse=always).
	CopyFile func(src, dst string) error
	// ReadyTimeout bounds waiting for a started VMM's API (default 15s).
	ReadyTimeout time.Duration
}

const (
	statePreparing   = "preparing"
	statePrepared    = vmlib.HostPrepared
	stateLaunching   = "launching"   // helper may have started the unit; identity unknown
	stateConfiguring = "configuring" // identity known; configuration may have reached the VMM
	stateRunning     = vmlib.HostRunning
	stateExiting     = vmlib.HostExiting
	stateExited      = vmlib.HostExited
)

type meta struct {
	AccessManaged bool   `json:"access_managed,omitempty"`
	AccessIntent  string `json:"access_intent,omitempty"`
	ID            string `json:"id"`
	ImageID       string `json:"image_id"`
	Profile       string `json:"profile"`
	Slot          int    `json:"slot"`
	// MemMiB and VCPU are the reservation the VM was ADMITTED with. They are
	// persisted so a later edit of the profile (or its removal) cannot change what
	// a live VM is counted or configured as. Zero = metadata from before
	// reservations existed; reconcile fills it conservatively.
	MemMiB uint32 `json:"mem_mib,omitempty"`
	VCPU   uint32 `json:"vcpu,omitempty"`
	// CPULimitMilli is the CPU-time quota (millicores) the VMM is started under,
	// persisted for the same reason. Zero = written before CPU limits existed;
	// it is then counted and enforced at VCPU*1000.
	CPULimitMilli uint32 `json:"cpu_limit_milli,omitempty"`
	// Zero denotes an old reservation, not permission to recompute a new bound.
	DiskMiB    uint32 `json:"disk_mib,omitempty"`
	State      string `json:"state"`
	Invocation string `json:"invocation,omitempty"`
	Retained   bool   `json:"retained,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type entry struct {
	mu   sync.Mutex
	m    meta
	dead bool // removed from the table; guarded by mu
}

var errStale = errors.New("local: workspace entry was replaced or removed")

// Backend implements vmlib.Backend on the local host.
type Backend struct {
	cfg Config

	mu      sync.Mutex
	entries map[string]*entry
	corrupt []string // unreadable workspaces: admission fails closed
	locks   []*os.File
	runFD   int // trusted runtime directory, opened O_NOFOLLOW; receipts are read relative to it

	// Lifecycle gate: every exported operation is admitted through enter() and
	// Close drains them before the ownership locks are released.
	gmu       sync.Mutex
	gcond     *sync.Cond
	active    int
	closing   bool
	closeCh   chan struct{} // closed when Close begins; wakes blocking waits
	closeDone chan struct{} // closed once the locks are released

	publishing map[string]struct{} // target image ids with a publisher in flight (guarded by mu)

	// hookPublishAfterMarker is a test seam between marker creation and the copy.
	hookPublishAfterMarker func(newID string)

	// hookAfterLookup is a test seam between entry lookup and lock acquisition.
	hookAfterLookup func(id string)
}

var _ vmlib.Backend = (*Backend)(nil)

func canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func devOf(p string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// checkStorage verifies the bounded-storage mount without writing anything.
func checkStorage(mount string, roots ...string) error {
	if !filepath.IsAbs(mount) {
		return errors.New("StorageMount must be absolute")
	}
	md, err := devOf(mount)
	if err != nil {
		return fmt.Errorf("storage mount: %w", err)
	}
	pd, err := devOf(filepath.Dir(filepath.Clean(mount)))
	if err != nil {
		return fmt.Errorf("storage mount parent: %w", err)
	}
	if md == pd {
		return fmt.Errorf("%s is not a mount point; refusing to fall back to the parent filesystem", mount)
	}
	for _, r := range roots {
		d, err := devOf(r)
		if err != nil {
			return err
		}
		if d != md {
			return fmt.Errorf("%s is not on the bounded storage mount %s", r, mount)
		}
	}
	return nil
}

func lockDir(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".backend.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another backend owns %s: %w", dir, err)
	}
	return f, nil // closing releases the lock; the inode is never unlinked
}

// New validates policy, takes exclusive ownership of the roots and reconciles
// existing host state, failing closed.
func New(cfg Config) (*Backend, error) {
	if err := cfg.PublicSSH.ValidateSlots(cfg.MaxVMs); err != nil {
		return nil, err
	}
	if cfg.PublicSSH != nil {
		policy := *cfg.PublicSSH
		cfg.PublicSSH = &policy
	}
	if cfg.MaxVMs < 1 || cfg.MaxVMs > maxSlots {
		return nil, fmt.Errorf("MaxVMs must be 1..%d", maxSlots)
	}
	if len(cfg.Profiles) == 0 || cfg.Launcher == nil || cfg.VMM == nil {
		return nil, errors.New("profiles, launcher and VMM controller are required")
	}
	if cfg.MaxTotalMemMiB == 0 || cfg.MaxTotalVCPU == 0 || cfg.UnitMemLimitMiB == 0 {
		return nil, errors.New("MaxTotalMemMiB, MaxTotalVCPU and UnitMemLimitMiB are required admission policy")
	}
	if err := vmhelper.ValidatePrefix(cfg.GuestPrefix); err != nil {
		return nil, fmt.Errorf("GuestPrefix is required: %w", err)
	}
	var pool vmhelper.Pool
	if cfg.PoolConf != "" {
		var err error
		pool, err = vmhelper.ReadPool(cfg.PoolConf)
		if err != nil {
			return nil, fmt.Errorf("pool configuration: %w", err)
		}
		cfg.PublicSSH, err = publicSSHFromPool(pool, cfg.PublicSSH, cfg.MaxVMs)
		if err != nil {
			return nil, err
		}
		if pool.Prefix != cfg.GuestPrefix || cfg.MaxVMs > pool.MaxSlots {
			return nil, fmt.Errorf("pool file (%s, %d slots) disagrees with backend policy (%s, %d VMs)", pool.Prefix, pool.MaxSlots, cfg.GuestPrefix, cfg.MaxVMs)
		}
	}
	if cfg.VMMOverheadMiB == 0 {
		cfg.VMMOverheadMiB = 64
	}
	if cfg.MaxTotalCPULimitMilli == 0 {
		if cfg.MaxTotalVCPU > math.MaxUint32/vmlib.CPUMilli {
			return nil, errors.New("MaxTotalVCPU is too large")
		}
		cfg.MaxTotalCPULimitMilli = cfg.MaxTotalVCPU * vmlib.CPUMilli
	}
	if cfg.MaxTotalCPULimitMilli == 0 || cfg.MaxTotalCPULimitMilli%vmlib.CPUQuotaStepMilli != 0 {
		return nil, errors.New("MaxTotalCPULimitMilli must be a positive multiple of 10")
	}
	// Normalize into a private copy: a profile without a CPU limit (policy that
	// predates cpu_limit) keeps meaning "the whole vCPU allowance".
	cfg.Profiles = append([]vmlib.Profile(nil), cfg.Profiles...)
	for i := range cfg.Profiles {
		if cfg.Profiles[i].CPULimitMilli == 0 && cfg.Profiles[i].VCPU > 0 && cfg.Profiles[i].VCPU <= vmlib.MaxVCPU {
			cfg.Profiles[i].CPULimitMilli = cfg.Profiles[i].VCPU * vmlib.CPUMilli
		}
	}
	cfg.GuestDNS = append([]string(nil), cfg.GuestDNS...)
	if cfg.GuestConfiguration == "" {
		cfg.GuestConfiguration = "mac-derived"
	}
	seenProfiles := map[string]bool{}
	for _, p := range cfg.Profiles {
		if seenProfiles[p.Name] {
			return nil, fmt.Errorf("duplicate profile %q", p.Name)
		}
		seenProfiles[p.Name] = true
		if cfg.PoolConf != "" {
			if p.CPULimitMilli > pool.MaxSlotCPUMilli {
				return nil, fmt.Errorf("profile %s exceeds pool CPU ceiling", p.Name)
			}
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if uint64(p.MemMiB)+uint64(cfg.VMMOverheadMiB) > uint64(cfg.UnitMemLimitMiB) {
			return nil, fmt.Errorf("profile %s (%d MiB + %d overhead) exceeds the installed per-unit limit %d MiB", p.Name, p.MemMiB, cfg.VMMOverheadMiB, cfg.UnitMemLimitMiB)
		}
		if p.MemMiB > cfg.MaxTotalMemMiB || p.VCPU > cfg.MaxTotalVCPU || p.CPULimitMilli > cfg.MaxTotalCPULimitMilli {
			return nil, fmt.Errorf("profile %s exceeds the aggregate budget", p.Name)
		}
	}
	if cfg.DefaultProfile == "" {
		cfg.DefaultProfile = cfg.Profiles[0].Name
		for _, p := range cfg.Profiles {
			if p.Name == "small" {
				cfg.DefaultProfile = "small"
			}
		}
	}
	if _, err := vmlib.ResolveProfile(cfg.Profiles, cfg.DefaultProfile); err != nil {
		return nil, fmt.Errorf("default profile: %w", err)
	}
	for _, p := range []string{cfg.WorkRoot, cfg.ImagesDir, cfg.RunDir} {
		if !filepath.IsAbs(p) {
			return nil, errors.New("WorkRoot, ImagesDir and RunDir must be absolute")
		}
	}
	work, err := canonical(cfg.WorkRoot)
	if err != nil {
		return nil, err
	}
	images, err := canonical(cfg.ImagesDir)
	if err != nil {
		return nil, err
	}
	run, err := canonical(cfg.RunDir)
	if err != nil {
		return nil, err
	}
	roots := []string{work, images, run}
	for i := range roots {
		for j := range roots {
			if i != j && within(roots[i], roots[j]) {
				return nil, fmt.Errorf("configured roots must not overlap: %s contains %s", roots[i], roots[j])
			}
		}
	}
	if cfg.StorageMount != "" {
		mount, err := canonical(cfg.StorageMount)
		if err != nil {
			return nil, fmt.Errorf("storage mount: %w", err)
		}
		if err := checkStorage(mount, work, images); err != nil {
			return nil, err
		}
	}
	cfg.WorkRoot, cfg.ImagesDir, cfg.RunDir = work, images, run
	// The runtime tree must be root-owned and not client-writable; refuse to start
	// (before anything is written) otherwise.
	if err := validateRunHierarchy(run, cfg.trustedUID(), cfg.TrustedAncestorRoot); err != nil {
		return nil, fmt.Errorf("unsafe runtime layout: %w", err)
	}
	if cfg.CopyFile == nil {
		cfg.CopyFile = copyFile
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 15 * time.Second
	}
	runFD, err := unix.Open(run, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open runtime directory: %w", err)
	}
	b := &Backend{cfg: cfg, runFD: runFD, entries: map[string]*entry{}, closeCh: make(chan struct{}), closeDone: make(chan struct{}), publishing: map[string]struct{}{}}
	b.gcond = sync.NewCond(&b.gmu)
	sorted := append([]string(nil), roots...)
	sort.Strings(sorted) // fixed order avoids lock-order deadlock between owners
	for _, dir := range sorted {
		var f *os.File
		var err error
		if dir == run {
			f, err = openRunLock(filepath.Join(run, ".backend.lock"), cfg.trustedUID())
		} else {
			f, err = lockDir(dir)
		}
		if err != nil {
			b.Close()
			return nil, err
		}
		b.locks = append(b.locks, f)
	}
	if err := os.MkdirAll(filepath.Join(work, "vms"), 0o770); err != nil {
		b.Close()
		return nil, err
	}
	// MkdirAll applies the caller's umask. The jailed VMM needs group traversal
	// even when the daemon runs with a restrictive UMask=0077.
	if err := os.Chmod(filepath.Join(work, "vms"), 0o770); err != nil {
		b.Close()
		return nil, err
	}
	if err := b.reconcile(); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

// ErrClosed is returned by operations attempted after Close has begun.
var ErrClosed = errors.New("local backend is closed or closing")

// enter admits one operation. It fails once Close has begun, so nothing new can
// start; the returned func must be called when the operation finishes. Internal
// helpers never call enter (only exported operations do), so nested calls cannot
// deadlock against Close.
func (b *Backend) enter() (func(), error) {
	b.gmu.Lock()
	defer b.gmu.Unlock()
	if b.closing {
		return nil, ErrClosed
	}
	b.active++
	return func() {
		b.gmu.Lock()
		b.active--
		if b.active == 0 {
			b.gcond.Broadcast()
		}
		b.gmu.Unlock()
	}, nil
}

// Close stops admitting operations, waits for every admitted operation to
// finish, and only then releases the ownership locks. It never stops a VM or
// removes state. Blocking waits (WaitExit) are woken and return ErrClosed;
// Start, once it has persisted its launch intent, always runs to completion, so
// Close can wait up to that operation's own bounded timeouts. Cancelling an
// operation's context is how a caller shortens that wait. Close is idempotent
// and safe to call concurrently; every call returns after the locks are released.
func (b *Backend) Close() error {
	b.gmu.Lock()
	first := !b.closing
	if first {
		b.closing = true
		close(b.closeCh)
	}
	for b.active > 0 {
		b.gcond.Wait()
	}
	b.gmu.Unlock()
	if !first {
		<-b.closeDone
		return nil
	}
	var err error
	b.mu.Lock()
	locks := b.locks
	b.locks = nil
	b.mu.Unlock()
	for _, f := range locks {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	unix.Close(b.runFD)
	close(b.closeDone)
	return err
}

func (b *Backend) vmDir(id string) string    { return filepath.Join(b.cfg.WorkRoot, "vms", id) }
func (b *Backend) diskPath(id string) string { return filepath.Join(b.vmDir(id), "disk.ext4") }

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

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
		// Retained leases keep their persisted slot even after admission changes.
		if err := b.cfg.PublicSSH.ValidateSlots(m.Slot + 1); err != nil {
			return fmt.Errorf("persisted SSH slot %d: %w", m.Slot, err)
		}
		e := &entry{m: m}
		if e.m.MemMiB == 0 || e.m.VCPU == 0 {
			lm, lv := b.largestProfile()
			if e.m.MemMiB == 0 {
				e.m.MemMiB = lm
			}
			if e.m.VCPU == 0 {
				e.m.VCPU = lv
			}
		}
		// Validate before deriving the legacy quota: an untrusted persisted
		// vCPU count must never overflow multiplication into a small reservation.
		if e.m.VCPU > vmlib.MaxVCPU {
			b.corrupt = append(b.corrupt, id)
			continue
		}
		if e.m.CPULimitMilli == 0 {
			e.m.CPULimitMilli = e.m.VCPU * vmlib.CPUMilli
		}
		if err := (vmlib.Profile{Name: "persisted", VCPU: e.m.VCPU, MemMiB: e.m.MemMiB, CPULimitMilli: e.m.CPULimitMilli}).Validate(); err != nil {
			b.corrupt = append(b.corrupt, id)
			continue
		}
		switch m.State {
		case stateLaunching, stateConfiguring, stateRunning, stateExiting:
			if _, done, _ := b.readReceipt(m); done {
				e.m.State, e.m.Retained, e.m.Reason = stateExited, false, ""
			} else if !e.m.Retained {
				e.m.Retained, e.m.Reason = true, "restart: VMM exit not proven; disk retained"
			}
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

// persist durably replaces the VM's metadata: fsync file, rename, fsync dir.
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
	if err := os.Rename(f.Name(), filepath.Join(dir, "meta.json")); err != nil {
		return err
	}
	return syncDir(dir)
}

func (b *Backend) profile(name string) (vmlib.Profile, bool) {
	for _, p := range b.cfg.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return vmlib.Profile{}, false
}

func (b *Backend) slotIP(slot int) string { return fmt.Sprintf("%s.%d.2", b.cfg.GuestPrefix, slot) }
func (b *Backend) slotGW(slot int) string { return fmt.Sprintf("%s.%d.1", b.cfg.GuestPrefix, slot) }
func (b *Backend) slotMAC(slot int) string {
	var a, c int
	fmt.Sscanf(b.cfg.GuestPrefix, "%d.%d", &a, &c)
	return fmt.Sprintf("06:00:%02X:%02X:%02X:02", a, c, slot)
}
func slotTap(slot int) string { return fmt.Sprintf("tap-vm%d", slot) }

// host maps internal states to the contract: retention is a FLAG over the real
// lifecycle state, never a state of its own.
func (b *Backend) host(m meta) vmlib.HostVM {
	state := m.State
	switch state {
	case statePreparing:
		state = vmlib.HostPrepared
	case stateLaunching, stateConfiguring:
		state = vmlib.HostRunning // a VMM may exist; shutdown is identity-checked
	}
	// The persisted bit distinguishes pre-access guests from managed guests even
	// after configuration changes or loss of a managed credential record.
	mode := vmlib.AuthImagePreinstalled
	if m.AccessManaged {
		mode = vmlib.AuthProvisionPublicKey
	}
	var endpoint *vmlib.SSHEndpoint
	if b.cfg.PublicSSH != nil {
		endpoint = &vmlib.SSHEndpoint{Host: b.cfg.PublicSSH.ReportedHost(), Port: b.cfg.PublicSSH.PortBase + m.Slot}
	}
	return vmlib.HostVM{SSHEndpoint: endpoint, SSHAuthMode: mode, ID: m.ID, ImageID: m.ImageID, Profile: m.Profile, Resources: vmlib.VMResources{VCPU: m.VCPU, CPULimitMilli: m.CPULimitMilli, MemMiB: m.MemMiB}, State: state, GuestIP: b.slotIP(m.Slot),
		Retained: m.Retained, RetainedReason: m.Reason, PublicKeyProvisioned: m.AccessManaged && m.State != statePreparing,
		Guest: &vmlib.GuestNetwork{Address: b.slotIP(m.Slot) + "/30", Gateway: b.slotGW(m.Slot),
			DNS: append([]string(nil), b.cfg.GuestDNS...), Configuration: b.cfg.GuestConfiguration}}
}

func (b *Backend) Capabilities(context.Context) (vmlib.Capabilities, error) {
	mode := vmlib.AuthImagePreinstalled
	if b.cfg.Access != nil {
		mode = vmlib.AuthProvisionPublicKey
	}
	return vmlib.Capabilities{MaxVMs: b.cfg.MaxVMs, Profiles: append([]vmlib.Profile(nil), b.cfg.Profiles...),
		Bastion: b.cfg.Bastion, SSHUser: b.cfg.SSHUser, SSHAuthMode: mode,
		MaxTotalMemMiB: b.cfg.MaxTotalMemMiB, MaxTotalVCPU: b.cfg.MaxTotalVCPU,
		MaxTotalCPULimitMilli: b.cfg.MaxTotalCPULimitMilli, DefaultProfile: b.cfg.DefaultProfile}, nil
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
	leave, gerr := b.enter()
	if gerr != nil {
		return nil, gerr
	}
	defer leave()
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
	leave, gerr := b.enter()
	if gerr != nil {
		return vmlib.Image{}, gerr
	}
	defer leave()
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

// largestProfile is the conservative stand-in for a reservation we cannot read.
func (b *Backend) largestProfile() (mem, vcpu uint32) {
	for _, q := range b.cfg.Profiles {
		if q.MemMiB > mem {
			mem = q.MemMiB
		}
		if q.VCPU > vcpu {
			vcpu = q.VCPU
		}
	}
	return
}

// leasedLocked sums the PERSISTED reservations of every leased VM (retained ones
// included). A VM without a reservation counts at the largest current profile.
func (b *Backend) leasedLocked() (mem, vcpu, cpu uint64) {
	lm, lv := b.largestProfile()
	for _, e := range b.entries {
		m, v, c := e.m.MemMiB, e.m.VCPU, e.m.CPULimitMilli
		if m == 0 {
			m = lm
		}
		if v == 0 {
			v = lv
		}
		if c == 0 {
			c = v * vmlib.CPUMilli
		}
		mem += uint64(m)
		vcpu += uint64(v)
		cpu += uint64(c)
	}
	return
}

// largestCPULimit is the highest configured per-VMM CPU limit.
func (b *Backend) largestCPULimit() (milli uint32) {
	for _, q := range b.cfg.Profiles {
		if q.CPULimitMilli > milli {
			milli = q.CPULimitMilli
		}
	}
	return
}

func (b *Backend) Prepare(ctx context.Context, req vmlib.PrepareRequest) (vmlib.HostVM, error) {
	leave, gerr := b.enter()
	if gerr != nil {
		return vmlib.HostVM{}, gerr
	}
	defer leave()
	for _, c := range [][2]string{{"vm", req.VMID}, {"image", req.ImageID}} {
		if err := vmlib.ValidateID(c[0], c[1]); err != nil {
			return vmlib.HostVM{}, err
		}
	}
	if req.SSHPublicKey != "" && b.cfg.Access == nil {
		return vmlib.HostVM{}, fmt.Errorf("%w: this backend does not provision SSH keys; use the image's preinstalled access", vmlib.ErrInvalidArgument)
	}
	if req.DiskMiB != 0 && (req.Resources == nil || req.Profile != "") {
		return vmlib.HostVM{}, vmlib.ErrInvalidArgument
	}
	if err := vmlib.ValidateResourceSelection(req.Profile, req.Resources); err != nil {
		return vmlib.HostVM{}, err
	}
	var src string
	if err := ctx.Err(); err != nil {
		return vmlib.HostVM{}, err
	}

	var e *entry
	for e == nil {
		b.mu.Lock()
		if len(b.corrupt) > 0 {
			b.mu.Unlock()
			return vmlib.HostVM{}, fmt.Errorf("host state unreadable for %d workspace(s); admission refused until an operator reviews them", len(b.corrupt))
		}
		if old, ok := b.entries[req.VMID]; ok {
			b.mu.Unlock()
			old.mu.Lock()
			if old.dead {
				old.mu.Unlock()
				continue // replaced while we waited; look again
			}
			defer old.mu.Unlock()
			if old.m.State == statePreparing {
				return vmlib.HostVM{}, errors.New("incomplete preparation retained; delete safely before retry")
			}
			if old.m.AccessManaged != (b.cfg.Access != nil) || old.m.AccessIntent != accessIntent(req.SSHPublicKey, b.cfg.Access != nil) {
				return vmlib.HostVM{}, fmt.Errorf("%w: VM access intent differs", vmlib.ErrAlreadyExists)
			}
			if old.m.ImageID == req.ImageID && old.m.Profile == req.Profile && (req.Resources == nil || (old.m.VCPU == req.Resources.VCPU && old.m.MemMiB == req.Resources.MemMiB && old.m.CPULimitMilli == req.Resources.EffectiveCPULimitMilli())) && (req.DiskMiB == 0 || old.m.DiskMiB == req.DiskMiB) {
				return b.host(old.m), nil
			}
			return vmlib.HostVM{}, fmt.Errorf("%w: vm %s", vmlib.ErrAlreadyExists, req.VMID)
		}
		prof, ok := b.profile(req.Profile)
		if req.Resources != nil {
			prof.VCPU, prof.MemMiB = req.Resources.VCPU, req.Resources.MemMiB
			if prof.VCPU > vmlib.MaxVCPU {
				b.mu.Unlock()
				return vmlib.HostVM{}, fmt.Errorf("%w: vcpu exceeds %d", vmlib.ErrInvalidArgument, vmlib.MaxVCPU)
			}
			prof.CPULimitMilli = req.Resources.EffectiveCPULimitMilli()
			if err := vmlib.ValidateCPULimitMilli(prof.CPULimitMilli); err != nil || prof.CPULimitMilli > prof.VCPU*vmlib.CPUMilli {
				b.mu.Unlock()
				return vmlib.HostVM{}, fmt.Errorf("%w: cpu_limit must be a representable limit not above the vCPU count", vmlib.ErrInvalidArgument)
			}
			// Explicit CPU/RAM does not expand disk authority: use the smallest profile disk bound.
			prof.DiskMiB = b.cfg.Profiles[0].DiskMiB
			for _, p := range b.cfg.Profiles {
				if p.DiskMiB < prof.DiskMiB {
					prof.DiskMiB = p.DiskMiB
				}
			}
			if req.DiskMiB != 0 {
				prof.DiskMiB = req.DiskMiB
			}
		} else if !ok {
			b.mu.Unlock()
			return vmlib.HostVM{}, fmt.Errorf("%w: unknown profile %q", vmlib.ErrInvalidArgument, req.Profile)
		}
		if req.Resources != nil && req.DiskMiB == 0 {
			// Legacy unsnapshotted resource requests retain their old maxima.
			mem, cpu := b.largestProfile()
			if req.Resources.VCPU > cpu || req.Resources.MemMiB > mem || prof.CPULimitMilli > b.largestCPULimit() {
				b.mu.Unlock()
				return vmlib.HostVM{}, fmt.Errorf("%w: CPU/RAM exceeds configured profile maxima", vmlib.ErrInvalidArgument)
			}
		}
		var info os.FileInfo
		var err error
		src, info, err = b.imagePath(req.ImageID)
		if err != nil {
			b.mu.Unlock()
			return vmlib.HostVM{}, err
		}
		if _, busy := b.publishing[req.ImageID]; busy {
			b.mu.Unlock()
			return vmlib.HostVM{}, fmt.Errorf("%w: image publication admission is reserved", vmlib.ErrResourceExhausted)
		}
		if uint64(info.Size()) > uint64(prof.DiskMiB)<<20 {
			b.mu.Unlock()
			return vmlib.HostVM{}, fmt.Errorf("%w: image larger than disk policy", vmlib.ErrInvalidArgument)
		}
		if len(b.entries) >= b.cfg.MaxVMs {
			b.mu.Unlock()
			return vmlib.HostVM{}, fmt.Errorf("%w: all %d VM slots are leased", vmlib.ErrResourceExhausted, b.cfg.MaxVMs)
		}
		mem, vcpu, cpu := b.leasedLocked()
		if mem+uint64(prof.MemMiB) > uint64(b.cfg.MaxTotalMemMiB) || vcpu+uint64(prof.VCPU) > uint64(b.cfg.MaxTotalVCPU) ||
			cpu+uint64(prof.CPULimitMilli) > uint64(b.cfg.MaxTotalCPULimitMilli) {
			b.mu.Unlock()
			return vmlib.HostVM{}, fmt.Errorf("%w: aggregate budget (%d/%d MiB, %d/%d vCPU, %d/%d millicores leased)", vmlib.ErrResourceExhausted,
				mem, b.cfg.MaxTotalMemMiB, vcpu, b.cfg.MaxTotalVCPU, cpu, b.cfg.MaxTotalCPULimitMilli)
		}
		used := map[int]bool{}
		for _, x := range b.entries {
			used[x.m.Slot] = true
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
		e = &entry{m: meta{ID: req.VMID, ImageID: req.ImageID, Profile: req.Profile, Slot: slot, State: statePreparing,
			MemMiB: prof.MemMiB, VCPU: prof.VCPU, CPULimitMilli: prof.CPULimitMilli, DiskMiB: prof.DiskMiB,
			AccessManaged: b.cfg.Access != nil, AccessIntent: accessIntent(req.SSHPublicKey, b.cfg.Access != nil)}}
		e.mu.Lock()
		b.entries[req.VMID] = e
		b.mu.Unlock()
	}
	defer e.mu.Unlock()

	fail := func(err error) (vmlib.HostVM, error) {
		os.RemoveAll(b.vmDir(req.VMID))
		b.mu.Lock()
		delete(b.entries, req.VMID)
		b.mu.Unlock()
		e.dead = true
		return vmlib.HostVM{}, err
	}
	if err := os.Mkdir(b.vmDir(req.VMID), 0o770); err != nil {
		b.mu.Lock()
		delete(b.entries, req.VMID)
		b.mu.Unlock()
		e.dead = true
		return vmlib.HostVM{}, err
	}
	if err := os.Chmod(b.vmDir(req.VMID), 0o770); err != nil {
		return fail(err)
	}
	if err := syncDir(filepath.Join(b.cfg.WorkRoot, "vms")); err != nil {
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
	if b.cfg.GuestNetwork != nil {
		if err := b.cfg.GuestNetwork.InstallNetwork(ctx, b.diskPath(req.VMID), *b.host(e.m).Guest); err != nil {
			return fail(fmt.Errorf("offline guest network install: %w", err))
		}
	}
	if b.cfg.Access != nil {
		if err := b.cfg.Access.Install(ctx, req.VMID, b.diskPath(req.VMID), req.SSHPublicKey); err != nil {
			return fail(fmt.Errorf("offline SSH install: %w", err))
		}
	}
	if f, err := os.OpenFile(b.diskPath(req.VMID), os.O_RDWR, 0); err == nil {
		err = f.Sync()
		f.Close()
		if err != nil {
			return fail(err)
		}
	}
	e.m.State = statePrepared
	if err := b.persist(&e.m); err != nil {
		return fail(err)
	}
	return b.host(e.m), nil
}

// lockEntry returns the VM's entry locked and verified to still be the current
// one. A stale pointer (removed or replaced after lookup) yields errStale, which
// guards against acting on a workspace that was re-created under the same ID.
func (b *Backend) lockEntry(id string) (*entry, error) {
	if err := vmlib.ValidateID("vm", id); err != nil {
		return nil, err
	}
	b.mu.Lock()
	e, ok := b.entries[id]
	b.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: vm %s", vmlib.ErrNotFound, id)
	}
	if b.hookAfterLookup != nil {
		b.hookAfterLookup(id)
	}
	e.mu.Lock()
	if e.dead {
		e.mu.Unlock()
		return nil, errStale
	}
	return e, nil
}

func staleToNotFound(id string, err error) error {
	if errors.Is(err, errStale) {
		return fmt.Errorf("%w: vm %s was replaced or removed", vmlib.ErrNotFound, id)
	}
	return err
}

func (b *Backend) Inspect(_ context.Context, id string) (vmlib.HostVM, error) {
	leave, gerr := b.enter()
	if gerr != nil {
		return vmlib.HostVM{}, gerr
	}
	defer leave()
	e, err := b.lockEntry(id)
	if err != nil {
		return vmlib.HostVM{}, staleToNotFound(id, err)
	}
	defer e.mu.Unlock()
	return b.host(e.m), nil
}

func (b *Backend) List(context.Context) ([]vmlib.HostVM, error) {
	leave, gerr := b.enter()
	if gerr != nil {
		return nil, gerr
	}
	defer leave()
	b.mu.Lock()
	es := make([]*entry, 0, len(b.entries))
	for _, e := range b.entries {
		es = append(es, e)
	}
	b.mu.Unlock()
	out := make([]vmlib.HostVM, 0, len(es))
	for _, e := range es {
		e.mu.Lock()
		if !e.dead {
			out = append(out, b.host(e.m))
		}
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

// Start persists the launch INTENT before the helper runs: from then on the
// disk may be held by a VMM and nothing may discard it until the VMM's identity
// is recorded and its exit receipt exists. After the intent is durable, Start
// runs to completion regardless of the caller's context (a cancelled helper
// invocation could lose its acknowledgement).
func (b *Backend) Start(ctx context.Context, id string) (vmlib.HostVM, error) {
	leave, gerr := b.enter()
	if gerr != nil {
		return vmlib.HostVM{}, gerr
	}
	defer leave()
	e, err := b.lockEntry(id)
	if err != nil {
		return vmlib.HostVM{}, staleToNotFound(id, err)
	}
	defer e.mu.Unlock()
	if e.m.State == stateRunning && !e.m.Retained {
		return b.host(e.m), nil // idempotent
	}
	if e.m.State != statePrepared || e.m.Retained || e.m.Invocation != "" {
		return vmlib.HostVM{}, fmt.Errorf("%w: VM %s cannot be started from state %s", vmlib.ErrNotStopped, id, e.m.State)
	}
	if err := ctx.Err(); err != nil {
		return vmlib.HostVM{}, err // nothing launched
	}
	e.m.State, e.m.Retained, e.m.Reason = stateLaunching, true, "launch requested; VMM identity not yet recorded"
	if err := b.persist(&e.m); err != nil {
		e.m.State, e.m.Retained, e.m.Reason = statePrepared, false, ""
		return vmlib.HostVM{}, fmt.Errorf("persist launch intent: %w", err) // helper was NOT called
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.ReadyTimeout+45*time.Second)
	defer cancel()
	if err := b.cfg.Launcher.StartSlot(dctx, e.m.Slot, e.m.CPULimitMilli); err != nil {
		e.m.Reason = "launcher reported an error but the unit may have started; disk retained: " + err.Error()
		_ = b.persist(&e.m)
		return b.host(e.m), fmt.Errorf("launcher: %w", err)
	}
	info, err := b.waitVMM(dctx, e.m.Slot)
	if err != nil {
		e.m.Reason = "VMM not usable after launch; disk retained: " + err.Error()
		_ = b.persist(&e.m)
		return b.host(e.m), err
	}
	e.m.Invocation, e.m.State, e.m.Reason = info.ID, stateConfiguring, "configuration in progress"
	if err := b.mustPersist(e); err != nil {
		return b.host(e.m), err
	}
	spec := MachineSpec{VCPU: e.m.VCPU, MemMiB: e.m.MemMiB, CPULimitMilli: e.m.CPULimitMilli, Kernel: "/kernel/vmlinux", BootArgs: "console=ttyS0 reboot=k panic=1 pci=off",
		DiskJailPath: "/work/vms/" + id + "/disk.ext4", Tap: slotTap(e.m.Slot), MAC: b.slotMAC(e.m.Slot)}
	if err := b.cfg.VMM.Configure(dctx, e.m.Slot, spec); err != nil {
		e.m.Reason = "VMM configuration failed; disk retained: " + err.Error()
		_ = b.persist(&e.m)
		return b.host(e.m), fmt.Errorf("configure VMM: %w", err)
	}
	cur, err := b.cfg.VMM.Info(dctx, e.m.Slot)
	if err != nil || cur.ID != e.m.Invocation {
		e.m.Reason = "VMM identity changed during configuration; disk retained"
		_ = b.persist(&e.m)
		return b.host(e.m), errors.New(e.m.Reason)
	}
	e.m.State, e.m.Reason = stateRunning, "InstanceStart response uncertain"
	_ = b.persist(&e.m)
	if err := b.cfg.VMM.InstanceStart(dctx, e.m.Slot); err != nil {
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
// The file is read only through the trusted runtime tree (see trust.go).
func (b *Backend) readReceipt(m meta) (clean, present bool, err error) {
	if !invocationPattern.MatchString(m.Invocation) {
		return false, false, nil
	}
	data, present, err := b.secureReceipt(m.Slot, m.Invocation)
	if err != nil || !present {
		return false, false, err
	}
	var r receiptFile
	if err := json.Unmarshal(data, &r); err != nil || r.InvocationID != m.Invocation {
		return false, false, errors.New("invalid exit receipt")
	}
	return r.Result == "success" && r.ExitCode == "exited" && r.ExitStatus == "0", true, nil
}

// RequestShutdown asks the ORIGINAL VMM to reboot-exit. It works for any VM
// whose invocation id is recorded (including retained/uncertain ones) and
// refuses when identity is unknown or a replacement answers.
func (b *Backend) RequestShutdown(ctx context.Context, id string) (vmlib.HostVM, error) {
	leave, gerr := b.enter()
	if gerr != nil {
		return vmlib.HostVM{}, gerr
	}
	defer leave()
	e, err := b.lockEntry(id)
	if err != nil {
		return vmlib.HostVM{}, staleToNotFound(id, err)
	}
	defer e.mu.Unlock()
	if e.m.State == stateExited || e.m.State == statePrepared || e.m.State == statePreparing {
		return b.host(e.m), nil
	}
	if e.m.Invocation == "" {
		return b.host(e.m), errors.New("VMM identity unknown (launch outcome uncertain); refusing to signal any process")
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
	cadErr := b.cfg.VMM.CtrlAltDel(ctx, e.m.Slot)
	if e.m.AccessManaged && b.cfg.CooperativeShutdown != nil {
		// CAD can have taken effect even on an error. Recheck the ORIGINAL
		// invocation before any second external effect; never signal a replacement.
		if _, done, _ := b.readReceipt(e.m); done {
			return b.markExitedLocked(e), nil
		}
		info, err = b.cfg.VMM.Info(ctx, e.m.Slot)
		if err != nil || info.ID != e.m.Invocation {
			return b.host(e.m), errors.New("cannot confirm original VMM identity after shutdown request")
		}
		if ctx.Err() == nil {
			attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = b.cfg.CooperativeShutdown.Reboot(attempt, e.m.ID, b.host(e.m).GuestIP, b.cfg.SSHUser)
			cancel()
		}
		// An SSH ack (including EOF) is not exit proof. Recheck identity even
		// after errors; a clean original receipt remains the only release proof.
		if _, done, _ := b.readReceipt(e.m); done {
			return b.markExitedLocked(e), nil
		}
		info, err = b.cfg.VMM.Info(ctx, e.m.Slot)
		if err != nil || info.ID != e.m.Invocation {
			return b.host(e.m), errors.New("cannot confirm original VMM identity after cooperative shutdown")
		}
		// With both optional paths attempted and identity still confirmed, keep
		// waiting/retrying for the receipt even if either acknowledgement was
		// lost or SSH is not ready. Disabled/legacy CAD error behavior is unchanged.
		cadErr = nil
	}
	if cadErr != nil {
		return b.host(e.m), fmt.Errorf("request guest shutdown: %w", cadErr)
	}
	if e.m.State == stateRunning || e.m.State == stateConfiguring {
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
	leave, gerr := b.enter()
	if gerr != nil {
		return vmlib.ExitReceipt{}, gerr
	}
	defer leave()
	for {
		e, err := b.lockEntry(id)
		if err != nil {
			return vmlib.ExitReceipt{}, staleToNotFound(id, err)
		}
		m := e.m
		e.mu.Unlock()
		if m.Invocation == "" {
			return vmlib.ExitReceipt{}, fmt.Errorf("%w: VM %s has no recorded VMM identity (never started or launch uncertain)", vmlib.ErrNotStopped, id)
		}
		clean, done, rerr := b.readReceipt(m)
		if rerr != nil {
			return vmlib.ExitReceipt{}, rerr
		}
		if done {
			if e, err := b.lockEntry(id); err == nil {
				b.markExitedLocked(e)
				e.mu.Unlock()
			}
			detail := ""
			if !clean {
				detail = "VMM did not exit cleanly"
			}
			return vmlib.ExitReceipt{VMID: id, Clean: clean, Detail: detail}, nil
		}
		select {
		case <-ctx.Done():
			return vmlib.ExitReceipt{}, fmt.Errorf("waiting for exit receipt of %s: %w", id, ctx.Err())
		case <-b.closeCh:
			return vmlib.ExitReceipt{}, fmt.Errorf("waiting for exit receipt of %s: %w", id, ErrClosed)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ---- publish / release --------------------------------------------------------

// Publish makes a NEW image visible atomically. The "committed" classification
// marker is made durable BEFORE the image appears, so an image is never visible
// without its classification; a failure at any step publishes nothing.
func (b *Backend) Publish(ctx context.Context, id, newID string) (vmlib.Image, error) {
	leave, gerr := b.enter()
	if gerr != nil {
		return vmlib.Image{}, gerr
	}
	defer leave()
	if err := vmlib.ValidateID("image", newID); err != nil {
		return vmlib.Image{}, err
	}
	// Reserve the TARGET across all entries: two publishers of one image id must
	// not share a marker, and a rollback may only remove its own artifacts.
	b.mu.Lock()
	if _, busy := b.publishing[newID]; busy {
		b.mu.Unlock()
		return vmlib.Image{}, fmt.Errorf("%w: image %s is being published by another operation", vmlib.ErrAlreadyExists, newID)
	}
	b.publishing[newID] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.publishing, newID)
		b.mu.Unlock()
	}()
	e, err := b.lockEntry(id)
	if err != nil {
		return vmlib.Image{}, staleToNotFound(id, err)
	}
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
	} else if !errors.Is(err, os.ErrNotExist) {
		return vmlib.Image{}, fmt.Errorf("inspect publication target: %w", err)
	}
	committedDir := filepath.Join(b.cfg.ImagesDir, ".committed")
	if err := os.MkdirAll(committedDir, 0o755); err != nil {
		return vmlib.Image{}, fmt.Errorf("committed-image catalog: %w", err)
	}
	marker := filepath.Join(committedDir, newID)
	// A marker without its final image is unresolved durable evidence, not a
	// reusable ID. Only an exclusively created marker belongs to this attempt.
	mf, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return vmlib.Image{}, fmt.Errorf("%w: image %s has an existing publication marker", vmlib.ErrAlreadyExists, newID)
		}
		return vmlib.Image{}, fmt.Errorf("write committed marker: %w", err)
	}
	merr := mf.Sync()
	if cerr := mf.Close(); merr == nil {
		merr = cerr
	}
	if merr == nil {
		merr = syncDir(committedDir)
	}
	if merr != nil {
		os.Remove(marker)
		return vmlib.Image{}, fmt.Errorf("persist committed marker: %w", merr)
	}
	if b.hookPublishAfterMarker != nil {
		b.hookPublishAfterMarker(newID)
	}
	published := false
	defer func() {
		if !published {
			if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
				os.Remove(marker)
			}
		}
	}()
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
	if e.m.AccessManaged {
		if b.cfg.Access == nil {
			return vmlib.Image{}, errors.New("managed SSH publication requires configured access provisioner")
		}
		if err := b.cfg.Access.ScrubVM(ctx, id, tmpPath); err != nil {
			return vmlib.Image{}, fmt.Errorf("offline SSH scrub: %w", err)
		}
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
	published = true
	if err := syncDir(b.cfg.ImagesDir); err != nil {
		// The image is visible but its directory entry is not proven durable.
		return vmlib.Image{}, fmt.Errorf("image %s published but directory sync failed: %w", newID, err)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		return vmlib.Image{}, err
	}
	return b.imageInfo(newID, info, true), nil
}

// Release deletes the workspace only with proof: never started, or the original
// VMM's exit receipt exists. A stale entry (already released, possibly re-created
// under the same ID) is reported as already released and never touched.
func (b *Backend) Release(_ context.Context, id string) (vmlib.ReleaseResult, error) {
	leave, gerr := b.enter()
	if gerr != nil {
		return vmlib.ReleaseResult{}, gerr
	}
	defer leave()
	e, err := b.lockEntry(id)
	if errors.Is(err, vmlib.ErrNotFound) || errors.Is(err, errStale) {
		// A stale lookup can name a replacement VM: do not delete its credentials.
		if errors.Is(err, vmlib.ErrNotFound) && b.cfg.Access != nil {
			b.mu.Lock()
			if b.entries[id] == nil {
				// An unreadable workspace is not proof of deletion. Also make
				// absence durable before discarding its credential record.
				if _, statErr := os.Lstat(b.vmDir(id)); !errors.Is(statErr, os.ErrNotExist) {
					err = errors.New("workspace absence not confirmed; credentials retained")
				} else if err = syncDir(filepath.Join(b.cfg.WorkRoot, "vms")); err == nil {
					err = b.cfg.Access.Delete(id)
				}
			} else {
				err = nil
			}
			b.mu.Unlock()
			if err != nil {
				return vmlib.ReleaseResult{}, err
			}
		}
		return vmlib.ReleaseResult{Released: true, Detail: "already released"}, nil
	}
	if err != nil {
		return vmlib.ReleaseResult{}, err
	}
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
	if err := syncDir(filepath.Join(b.cfg.WorkRoot, "vms")); err != nil {
		return vmlib.ReleaseResult{}, err
	}
	if b.cfg.Access != nil {
		if err := b.cfg.Access.Delete(id); err != nil {
			return vmlib.ReleaseResult{}, err
		}
	}
	b.mu.Lock()
	if b.entries[id] == e {
		delete(b.entries, id)
	}
	b.mu.Unlock()
	e.dead = true
	return vmlib.ReleaseResult{Released: true}, nil
}
