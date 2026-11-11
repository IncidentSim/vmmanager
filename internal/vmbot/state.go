// Package vmbot implements the multi-VM Telegram bot application: durable
// state, the controller over vmlib.Lifecycle, the Telegram front end and the
// capability-link web terminal. See docs/adr/0002-telegram-single-vm-bot.md and
// docs/adr/0003-vmbot-multi-vm.md.
package vmbot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// Application-level VM phases. A VM in any phase occupies one slot.
const (
	PhaseCreating = "creating" // identity persisted, Create outcome not yet known
	PhaseActive   = "active"
	PhaseDeleting = "deleting" // terminal revoked for good, deletion not yet confirmed
)

const (
	stateVersion   = 2
	stateVersionV1 = 1
	// MaxVMsLimit is the hard ceiling for the configurable max_vms.
	MaxVMsLimit = 8
)

var ErrLocked = errors.New("vmbot: state directory is locked by another controller")

// labelPattern is deliberately conservative: lowercase ASCII, digits and inner
// hyphens. A label is display/selection data only and never reaches a path,
// socket name or command line.
var labelPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$`)

// ValidateLabel checks a user-supplied VM label.
func ValidateLabel(l string) error {
	if !labelPattern.MatchString(l) || strings.HasPrefix(l, "bot-") {
		return errors.New("invalid label")
	}
	return nil
}

// VMRecord is one VM this application owns.
type VMRecord struct {
	ID         string `json:"id"`
	Token      string `json:"token"` // terminal capability (secret)
	Phase      string `json:"phase"`
	Generation uint64 `json:"generation"`
	Label      string `json:"label,omitempty"` // unique, immutable display name
	// Seq is the delete-attempt sequence (ADR 0004): the durable part of the
	// delete workflow ID. 0 = no delete intent yet. A retry after a finished,
	// unsuccessful attempt persists Seq+1 before a new workflow is submitted.
	Seq         uint64            `json:"seq,omitempty"`
	OwnerUserID int64             `json:"owner_user_id,omitempty"`
	ImageID     string            `json:"image_id,omitempty"`
	Resources   *ControlResources `json:"resources,omitempty"`
	// DiskMiB is the immutable admission disk bound; zero retains legacy policy.
	DiskMiB      uint32 `json:"disk_mib,omitempty"`
	Profile      string `json:"profile,omitempty"`
	SSHPublicKey string `json:"ssh_public_key,omitempty"`
	RemoteState  string `json:"remote_state,omitempty"`
	// TTLSeconds and ExpiresAt are set once at Create admission and never change
	// (no renewal, restart does not reset them). A record without ExpiresAt is a
	// legacy VM: it has no deadline and never receives one retroactively.
	TTLSeconds int64      `json:"ttl_seconds,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// State is persisted as state.json (schema v2).
type State struct {
	AdminBatches []AdminBatch       `json:"admin_batches,omitempty"`
	Control      bool               `json:"control,omitempty"`
	Operations   []ControlOperation `json:"operations,omitempty"`
	Images       []ControlImage     `json:"images,omitempty"`
	Version      int                `json:"version"`
	Generation   uint64             `json:"generation"` // monotonic high-water mark; never reused
	VMs          []VMRecord         `json:"vms"`        // ascending Generation
	Offset       int64              `json:"offset"`     // next Telegram update_id to request
}

// stateV1 is the previous single-VM schema, read only for migration.
type stateV1 struct {
	Version    int       `json:"version"`
	Generation uint64    `json:"generation"`
	VM         *VMRecord `json:"vm,omitempty"`
	Offset     int64     `json:"offset"`
}

// ByID returns the record with the VM ID, if any.
func (s State) ByID(id string) (VMRecord, bool) {
	for _, r := range s.VMs {
		if r.ID == id {
			return r, true
		}
	}
	return VMRecord{}, false
}

func (s *State) update(id string, f func(*VMRecord)) {
	for i := range s.VMs {
		if s.VMs[i].ID == id {
			f(&s.VMs[i])
			return
		}
	}
}

func (s *State) remove(id string) {
	out := s.VMs[:0]
	for _, r := range s.VMs {
		if r.ID != id {
			out = append(out, r)
		}
	}
	s.VMs = out
}

// validate checks every invariant. It never mutates.
func (s State) validate() error {
	if s.Version != stateVersion {
		return fmt.Errorf("state version %d unsupported", s.Version)
	}
	if s.Offset < 0 {
		return errors.New("invalid state: negative update offset")
	}
	ids, toks, labels := map[string]bool{}, map[string]bool{}, map[string]bool{}
	if s.Control {
		if err := validateControlState(s); err != nil {
			return err
		}
	}
	var prev uint64
	for _, r := range s.VMs {
		if err := vmlib.ValidateID("vm", r.ID); err != nil {
			return fmt.Errorf("invalid state: bad VM id: %w", err)
		}
		if !tokenPattern.MatchString(r.Token) {
			return errors.New("invalid state: malformed capability token")
		}
		switch r.Phase {
		case PhaseCreating, PhaseActive, PhaseDeleting, "stopping", "stopped", "committing", "committed":
		default:
			return fmt.Errorf("invalid state: unknown phase %q", r.Phase)
		}
		if r.Generation == 0 || r.Generation > s.Generation {
			return errors.New("invalid state: generation out of range")
		}
		if r.Generation <= prev {
			return errors.New("invalid state: generations must be unique and ascending")
		}
		prev = r.Generation
		if r.Label != "" {
			if err := ValidateLabel(r.Label); err != nil {
				return errors.New("invalid state: bad label")
			}
			labelKey := r.Label
			if s.Control {
				labelKey = fmt.Sprintf("%d:%s", r.OwnerUserID, r.Label)
			}
			if labels[labelKey] {
				return errors.New("invalid state: duplicate label")
			}
			labels[labelKey] = true
		}
		if ids[r.ID] || toks[r.Token] {
			return errors.New("invalid state: duplicate VM id or token")
		}
		ids[r.ID], toks[r.Token] = true, true
	}
	return nil
}

// MigrationInfo describes a v1 -> v2 migration performed by OpenStore.
type MigrationInfo struct {
	BackupPath string // exact v1 bytes, mode 0600
}

// Store is the durable application state guarded by an exclusive lifetime flock.
type Store struct {
	mu          sync.Mutex
	dir         string
	lock        *os.File
	engineOwner *Controller // keeps the flock alive while DBOS may execute work
	st          State
	migration   *MigrationInfo
	controlOpen bool
	legacyOwner int64
}

// OpenStore creates dir (0700) if needed, takes the exclusive lock and loads
// state, migrating a v1 file to v2. It fails closed on loose permissions,
// corrupt, invalid or future-version data.
func OpenStore(dir string) (*Store, error) { return openStore(dir, false, 0) }
func openStore(dir string, control bool, owner int64) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("state dir: not a directory")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("state dir %s must not be accessible by group/other (mode %o)", dir, fi.Mode().Perm())
	}
	lf, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state lock: %w", err)
	}
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lf.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("state lock: %w", err)
	}
	s := &Store{dir: dir, lock: lf, controlOpen: control, legacyOwner: owner}
	if err := s.load(); err != nil {
		lf.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "state.json") }

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

func (s *Store) load() error {
	fi, err := os.Stat(s.path())
	if errors.Is(err, os.ErrNotExist) {
		s.st = State{Version: stateVersion, VMs: []VMRecord{}}
		return nil
	}
	if err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state file must be mode 0600 (is %o)", fi.Mode().Perm())
	}
	b, err := os.ReadFile(s.path())
	if err != nil {
		return fmt.Errorf("state file: %w", err)
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return fmt.Errorf("state file is corrupt; refusing to continue: %w", err)
	}
	switch probe.Version {
	case stateVersion:
		var st State
		if err := decodeStrict(b, &st); err != nil {
			return fmt.Errorf("state file is corrupt; refusing to continue: %w", err)
		}
		if st.VMs == nil {
			st.VMs = []VMRecord{}
		}
		if err := st.validate(); err != nil {
			return fmt.Errorf("state file: %w", err)
		}
		if s.controlOpen && !st.Control && len(st.VMs) > 0 && s.legacyOwner <= 0 {
			return errors.New("legacy_owner_user_id required")
		}
		s.st = st
		return nil
	case stateVersionV1:
		return s.migrateV1(b)
	default:
		return fmt.Errorf("state file version %d unsupported", probe.Version)
	}
}

// migrateV1 validates the v1 bytes completely, preserves them in a private
// backup that is never overwritten, then atomically installs the v2 file.
func (s *Store) migrateV1(raw []byte) error {
	var v1 stateV1
	if err := decodeStrict(raw, &v1); err != nil {
		return fmt.Errorf("state file is corrupt; refusing to continue: %w", err)
	}
	next := State{Version: stateVersion, Generation: v1.Generation, Offset: v1.Offset, VMs: []VMRecord{}}
	if s.controlOpen && v1.VM != nil && s.legacyOwner <= 0 {
		return errors.New("legacy_owner_user_id required")
	}
	if v1.VM != nil {
		r := *v1.VM
		if r.Label != "" {
			return errors.New("state file (v1): unexpected label")
		}
		r.Label = fmt.Sprintf("vm-%d", r.Generation)
		next.VMs = append(next.VMs, r)
	}
	if err := next.validate(); err != nil {
		return fmt.Errorf("state file (v1): %w", err)
	}
	bak, err := s.preserveBackup(raw)
	if err != nil {
		return fmt.Errorf("state migration aborted (no backup): %w", err)
	}
	if err := s.write(next); err != nil {
		return fmt.Errorf("state migration: %w", err)
	}
	s.st = next
	s.migration = &MigrationInfo{BackupPath: bak}
	return nil
}

// preserveBackup stores raw as state.json.v1.bak (then .1, .2, ...). An existing
// file is never overwritten; an identical earlier backup (a retry) is reused.
func (s *Store) preserveBackup(raw []byte) (string, error) {
	base := filepath.Join(s.dir, "state.json.v1.bak")
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s.%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			if old, rerr := os.ReadFile(name); rerr == nil && bytes.Equal(old, raw) {
				return name, nil
			}
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(raw)
		if werr == nil {
			werr = f.Sync()
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(name)
			return "", werr
		}
		if err := syncDir(s.dir); err != nil {
			return "", err
		}
		return name, nil
	}
	return "", errors.New("too many backups")
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Migration reports a migration done by OpenStore, or nil.
func (s *Store) Migration() *MigrationInfo { return s.migration }

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.st)
}

func clone(st State) State {
	st.AdminBatches = append([]AdminBatch{}, st.AdminBatches...)
	for i := range st.AdminBatches {
		st.AdminBatches[i].VMs = append([]AdminVMRef{}, st.AdminBatches[i].VMs...)
	}
	st.Operations = append([]ControlOperation{}, st.Operations...)
	st.Images = append([]ControlImage{}, st.Images...)
	st.VMs = append([]VMRecord{}, st.VMs...)
	for n := range st.VMs {
		if st.VMs[n].Resources != nil {
			r := *st.VMs[n].Resources
			st.VMs[n].Resources = &r
		}
		if st.VMs[n].ExpiresAt != nil {
			t := *st.VMs[n].ExpiresAt
			st.VMs[n].ExpiresAt = &t
		}
	}
	return st
}

// Update applies mutate and persists atomically. When validation or
// persistence fails the in-memory state is left unchanged.
func (s *Store) Update(mutate func(*State)) error {
	return s.UpdateErr(func(st *State) error { mutate(st); return nil })
}

// UpdateErr is Update where mutate may refuse; a refusal changes nothing and is
// returned as is. Checks done inside mutate are atomic with the write.
func (s *Store) UpdateErr(mutate func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.st)
	if err := mutate(&next); err != nil {
		return err
	}
	if s.st.Control {
		if e := immutableAdminBatches(s.st.AdminBatches, next.AdminBatches); e != nil {
			return e
		}
		for _, old := range s.st.VMs {
			if cur, ok := next.ByID(old.ID); ok && cur.OwnerUserID != old.OwnerUserID {
				return errors.New("VM owner is immutable")
			}
		}
		for _, old := range s.st.Images {
			for _, cur := range next.Images {
				if cur.ID == old.ID && cur.OwnerUserID != old.OwnerUserID {
					return errors.New("image owner is immutable")
				}
			}
		}
		for _, old := range s.st.Operations {
			for _, cur := range next.Operations {
				if cur.ID == old.ID && cur.OwnerUserID != old.OwnerUserID {
					return errors.New("operation owner is immutable")
				}
			}
		}
	}
	next.Version = stateVersion
	sort.Slice(next.VMs, func(i, j int) bool { return next.VMs[i].Generation < next.VMs[j].Generation })
	if err := next.validate(); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	if err := s.write(next); err != nil {
		return err
	}
	s.st = clone(next)
	return nil
}

func (s *Store) write(st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "state-*.tmp")
	if err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("persist state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(b); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("persist state: %w", err)
	}
	if err := os.Rename(name, s.path()); err != nil {
		os.Remove(name)
		return fmt.Errorf("persist state: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	return nil
}

func (s *Store) claimEngine(c *Controller) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil || s.engineOwner != nil {
		return errors.New("orchestration requires an exclusively owned open store")
	}
	s.engineOwner = c
	return nil
}

func (s *Store) releaseEngine(c *Controller) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engineOwner == c {
		s.engineOwner = nil
	}
}

// Close releases the lifetime lock only after orchestration has fully stopped.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engineOwner != nil {
		return errors.New("orchestration still owns the store lock")
	}
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}
