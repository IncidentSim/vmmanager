package tgbot

import (
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"sync"
)

type deliveryState struct {
	AdminReports []string `json:"admin_reports,omitempty"`
	Version      int      `json:"version"`
	Offset       int64    `json:"offset"`
	Imported     bool     `json:"imported"`
}
type Delivery struct {
	mu          sync.Mutex
	dir         string
	lock        *os.File
	state       deliveryState
	initialized bool
}

func OpenDelivery(dir string) (*Delivery, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, errors.New("cannot create delivery directory")
	}
	fi, e := os.Lstat(dir)
	var st unix.Stat_t
	se := unix.Lstat(dir, &st)
	if e != nil || se != nil || !fi.IsDir() || fi.Mode().Perm()&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("delivery directory must be private and owned")
	}
	// A separate namespace is mandatory: never open a legacy/API state directory.
	for _, p := range []string{"state.json", "dbos.sqlite", "dbos.sqlite3"} {
		if _, e = os.Lstat(filepath.Join(dir, p)); !os.IsNotExist(e) {
			return nil, errors.New("delivery directory contains application state; use a separate directory")
		}
	}
	f, e := privateOpen(filepath.Join(dir, "delivery.lock"), unix.O_RDWR|unix.O_CREAT)
	if e != nil {
		return nil, e
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		f.Close()
		return nil, errors.New("delivery directory is locked")
	}
	s := &Delivery{dir: dir, lock: f, state: deliveryState{Version: 1}}
	path := filepath.Join(dir, "delivery.json")
	if _, e = os.Lstat(path); os.IsNotExist(e) {
		return s, nil
	}
	b, e := privateRead(path)
	var wire struct {
		AdminReports []string `json:"admin_reports,omitempty"`
		Version      *int     `json:"version"`
		Offset       *int64   `json:"offset"`
		Imported     *bool    `json:"imported"`
	}
	if e != nil || strict(b, &wire) != nil || wire.Version == nil || *wire.Version != 1 || wire.Offset == nil || *wire.Offset < 0 || wire.Imported == nil {
		s.Close()
		return nil, errors.New("invalid private delivery state")
	}
	s.state = deliveryState{Version: *wire.Version, Offset: *wire.Offset, Imported: *wire.Imported, AdminReports: wire.AdminReports}
	s.initialized = true
	return s, nil
}
func (s *Delivery) Offset() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Offset }
func (s *Delivery) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	e := s.lock.Close()
	s.lock = nil
	return e
}
func (s *Delivery) persist(next deliveryState) error {
	if s.lock == nil {
		return errors.New("delivery store closed")
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(s.dir, "delivery-*.tmp")
	if e != nil {
		return errors.New("cannot persist delivery offset")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(name, filepath.Join(s.dir, "delivery.json"))
	}
	if e == nil {
		var d *os.File
		d, e = os.Open(s.dir)
		if e == nil {
			e = d.Sync()
			d.Close()
		}
	}
	if e != nil {
		return errors.New("cannot persist delivery offset")
	}
	s.state = next
	s.initialized = true
	return nil
}
func (s *Delivery) Advance(next int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next <= s.state.Offset {
		return errors.New("delivery offset must increase")
	}
	n := s.state
	n.Offset = next
	return s.persist(n)
}

// ImportLegacy only reads an explicitly named private v1/v2 source. The
// operator must stop the legacy service first. Its existing lock is held while
// taking this snapshot; the original state/VM records are never rewritten.
func (s *Delivery) ImportLegacy(source string, legacyStopped bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !legacyStopped || s.initialized {
		return errors.New("import requires stopped legacy and unused delivery store")
	}
	lf, e := privateOpen(filepath.Join(filepath.Dir(source), "lock"), unix.O_RDONLY)
	if e != nil {
		return e
	}
	defer lf.Close()
	if unix.Flock(int(lf.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return errors.New("legacy controller still holds state lock")
	}
	b, e := privateRead(source)
	if e != nil {
		return e
	}
	var old struct {
		Version int    `json:"version"`
		Offset  *int64 `json:"offset"`
	}
	if !uniqueObject(b) || json.Unmarshal(b, &old) != nil || (old.Version != 1 && old.Version != 2) || old.Offset == nil || *old.Offset < 0 {
		return errors.New("invalid legacy v1/v2 offset")
	}
	return s.persist(deliveryState{Version: 1, Offset: *old.Offset, Imported: true})
}
