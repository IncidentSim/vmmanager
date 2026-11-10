// Package vmbot implements the single-VM Telegram bot application: durable
// state, the controller over vmlib.Lifecycle, the Telegram front end and the
// capability-link web terminal. See docs/adr/0002-telegram-single-vm-bot.md.
package vmbot

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// Application-level VM phases. A VM in any phase occupies the single slot.
const (
	PhaseCreating = "creating" // identity persisted, Create outcome not yet known
	PhaseActive   = "active"
	PhaseDeleting = "deleting" // terminal revoked for good, deletion not yet confirmed
)

const stateVersion = 1

var ErrLocked = errors.New("vmbot: state directory is locked by another controller")

// VMRecord is the one VM this application owns.
type VMRecord struct {
	ID         string `json:"id"`
	Token      string `json:"token"` // terminal capability (secret)
	Phase      string `json:"phase"`
	Generation uint64 `json:"generation"`
}

// State is persisted as state.json.
type State struct {
	Version    int       `json:"version"`
	Generation uint64    `json:"generation"` // monotonic; never reused
	VM         *VMRecord `json:"vm,omitempty"`
	Offset     int64     `json:"offset"` // next Telegram update_id to request
}

// Store is the durable application state guarded by an exclusive lifetime flock.
type Store struct {
	mu   sync.Mutex
	dir  string
	lock *os.File
	st   State
}

// OpenStore creates dir (0700) if needed, takes the exclusive lock and loads
// state. It fails closed on loose permissions, corrupt or future-version data.
func OpenStore(dir string) (*Store, error) {
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
	s := &Store{dir: dir, lock: lf}
	if err := s.load(); err != nil {
		lf.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "state.json") }

func (s *Store) load() error {
	fi, err := os.Stat(s.path())
	if errors.Is(err, os.ErrNotExist) {
		s.st = State{Version: stateVersion}
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
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("state file is corrupt; refusing to continue: %w", err)
	}
	if st.Version != stateVersion {
		return fmt.Errorf("state file version %d unsupported", st.Version)
	}
	s.st = st
	return nil
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.st)
}

func clone(st State) State {
	if st.VM != nil {
		v := *st.VM
		st.VM = &v
	}
	return st
}

// Update applies mutate and persists atomically. When persistence fails the
// in-memory state is left unchanged.
func (s *Store) Update(mutate func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.st)
	mutate(&next)
	next.Version = stateVersion
	if err := s.write(next); err != nil {
		return err
	}
	s.st = next
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
	d, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	defer d.Close()
	return d.Sync()
}

// Close releases the lifetime lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return err
}
