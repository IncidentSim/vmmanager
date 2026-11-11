package nextermaccess

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Stages of one per-VM binding.
const (
	StagePlanned     = "planned"     // credentials generated; Nexterm objects may be partial
	StageReady       = "ready"       // account, identity and entry verified
	StageUnsupported = "unsupported" // VM has no generated key (user-supplied key)
)

// Record is the durable owner+VM binding. The generated password lives only in
// this 0600 file (needed for retry/reissue); the SSH private key never does.
type Record struct {
	VMID       string `json:"vm_id"`
	Owner      int64  `json:"owner"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	UserID     int64  `json:"user_id,omitempty"`
	IdentityID int64  `json:"identity_id,omitempty"`
	EntryID    int64  `json:"entry_id,omitempty"`
	// LinkToken is the one per-user (never admin) session reused for the
	// auto-login link; it is closed individually on cleanup.
	LinkToken string    `json:"link_token,omitempty"`
	Stage     string    `json:"stage"`
	Delivered bool      `json:"delivered,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Updated   time.Time `json:"updated"`
}

// String never reveals the password.
func (r Record) String() string { return "nexterm-binding{" + r.VMID + "," + r.Stage + "}" }

type storeFile struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

type Store struct {
	mu   sync.Mutex
	path string
	lock *os.File
	recs map[string]Record
}

func openPrivate(path string, flags int) (*os.File, error) {
	fd, e := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return nil, errors.New("cannot open private file")
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, errors.New("private file must be an owned regular file with mode 0600")
	}
	return f, nil
}

// ReadPrivate reads a size-bounded owned 0600 regular file without following a
// final symlink.
func ReadPrivate(path string) ([]byte, error) {
	f, e := openPrivate(path, unix.O_RDONLY)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if e != nil || len(b) > 4<<20 {
		return nil, errors.New("invalid private file size")
	}
	return b, nil
}

func checkDir(dir string) error {
	var st unix.Stat_t
	if unix.Lstat(dir, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0077 != 0 || st.Uid != uint32(os.Geteuid()) {
		return errors.New("nexterm state directory must exist, be owned, mode 0700 and not a symlink")
	}
	return nil
}

// OpenStore opens (or initialises) the state file under an exclusive process lock.
func OpenStore(path string) (*Store, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("nexterm state path must be absolute and clean")
	}
	dir := filepath.Dir(path)
	if e := checkDir(dir); e != nil {
		return nil, e
	}
	lf, e := openPrivate(path+".lock", unix.O_RDWR|unix.O_CREAT)
	if e != nil {
		return nil, e
	}
	if unix.Flock(int(lf.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		lf.Close()
		return nil, errors.New("nexterm state is locked by another process")
	}
	s := &Store{path: path, lock: lf, recs: map[string]Record{}}
	var st unix.Stat_t
	if e := unix.Lstat(path, &st); e != nil {
		if e == unix.ENOENT {
			return s, nil
		}
		lf.Close()
		return nil, errors.New("cannot stat nexterm state")
	}
	b, e := ReadPrivate(path)
	if e != nil {
		lf.Close()
		return nil, e
	}
	var sf storeFile
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&sf) != nil || sf.Version != 1 || d.Decode(new(any)) != io.EOF {
		lf.Close()
		return nil, errors.New("invalid nexterm state")
	}
	for _, r := range sf.Records {
		if r.VMID == "" || r.Owner <= 0 || s.recs[r.VMID].VMID != "" || (r.Stage != StagePlanned && r.Stage != StageReady && r.Stage != StageUnsupported) {
			lf.Close()
			return nil, errors.New("invalid nexterm state record")
		}
		s.recs[r.VMID] = r
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	e := s.lock.Close()
	s.lock = nil
	return e
}

func (s *Store) Get(vm string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[vm]
	return r, ok
}

func (s *Store) All() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out
}

// Put durably replaces one record; memory changes only if the write succeeded.
func (s *Store) Put(r Record) error { return s.mutate(func(m map[string]Record) { m[r.VMID] = r }) }
func (s *Store) Delete(vm string) error {
	return s.mutate(func(m map[string]Record) { delete(m, vm) })
}

func (s *Store) mutate(f func(map[string]Record)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return errors.New("nexterm state closed")
	}
	next := make(map[string]Record, len(s.recs)+1)
	for k, v := range s.recs {
		next[k] = v
	}
	f(next)
	sf := storeFile{Version: 1}
	for _, r := range next {
		sf.Records = append(sf.Records, r)
	}
	sort.Slice(sf.Records, func(i, j int) bool { return sf.Records[i].VMID < sf.Records[j].VMID })
	b, e := json.Marshal(sf)
	if e != nil {
		return e
	}
	dir := filepath.Dir(s.path)
	tmp := filepath.Join(dir, ".nexterm-state.tmp")
	_ = os.Remove(tmp)
	fd, e := unix.Open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return errors.New("cannot persist nexterm state")
	}
	f2 := os.NewFile(uintptr(fd), tmp)
	_, e = f2.Write(b)
	if e == nil {
		e = f2.Sync()
	}
	if ce := f2.Close(); e == nil {
		e = ce
	}
	if e == nil {
		e = os.Rename(tmp, s.path)
	}
	if e == nil {
		if d, de := os.Open(dir); de == nil {
			e = d.Sync()
			d.Close()
		}
	}
	if e != nil {
		_ = os.Remove(tmp)
		return errors.New("cannot persist nexterm state")
	}
	s.recs = next
	return nil
}
