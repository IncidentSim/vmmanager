// Package identity holds the Telegram username policy shared by the bot and the
// control API: canonical username handling and the durable first-claim binding
// of a configured username slot to one immutable numeric Telegram user ID.
package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ErrBindingConflict means the username slot already belongs to another user ID.
var ErrBindingConflict = errors.New("username is bound to a different Telegram user")

var errLockBusy = errors.New("username bindings are busy")

var usernamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{4,31}$`)

// CanonicalUsername validates Telegram's ASCII username grammar (5-32 characters,
// letters/digits/underscore, starting with a letter) after stripping at most one
// leading "@", and returns the case-folded form. Telegram usernames compare
// case-insensitively.
func CanonicalUsername(s string) (string, error) {
	s = strings.TrimPrefix(s, "@")
	if !usernamePattern.MatchString(s) {
		return "", errors.New("invalid Telegram username")
	}
	return strings.ToLower(s), nil
}

// CanonicalList canonicalizes configured usernames and rejects invalid names and
// case-insensitive duplicates. An empty list is valid.
func CanonicalList(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		n, err := CanonicalUsername(raw)
		if err != nil {
			return nil, err
		}
		if seen[n] {
			return nil, errors.New("duplicate Telegram username")
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

const maxBindingFile = 1 << 20

// lockWait bounds how long an authority read/write waits for the cross-process
// lock. Timing out fails closed (no authority, claim error).
var lockWait = 2 * time.Second

type fileSig struct {
	ino, size int64
	mod       int64
	exists    bool
}

// Bindings is the permanent username -> user ID map.
//
// Cross-process protocol (bot and API use the same code on their own files):
// EVERY authority read (Owner, Names, BoundTo, and the check inside Claim) and
// every write happens under an flock on a sibling lock file - shared for reads,
// exclusive for Claim - and a loaded snapshot is only trusted after durability of
// exactly that file version has been positively established under the lock: the
// file is fsynced and its directory is fsynced successfully (syncDirHook). A
// visible-but-unsynced rename (writer still syncing, or a failed directory sync
// left by any process) therefore never grants authority: readers wait behind the
// writer's exclusive lock (bounded, then fail closed) and a later reader or
// retry must itself complete the directory fsync before it may trust the file.
// Only a successfully synced file signature is cached; any other signature is
// re-read and re-synced. Bindings never calls back into other components while
// holding its locks.
type Bindings struct {
	path, lockPath string
	mu             sync.Mutex
	byName         map[string]int64
	verified       fileSig // signature whose durability was established
}

// OpenBindings validates (without creating or trusting) the binding file: a
// corrupt or unsafe file is an error. It grants no authority and takes no lock;
// authority is only ever derived under the lock protocol by the methods below.
func OpenBindings(path string) (*Bindings, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("binding path must be absolute")
	}
	b := &Bindings{path: path, lockPath: path + ".lock", byName: map[string]int64{}}
	_, _, f, err := b.readFile()
	if err != nil {
		return nil, err
	}
	if f != nil {
		if err := f.Close(); err != nil {
			return nil, errors.New("cannot close username bindings")
		}
	}
	return b, nil
}

func statSig(path string) (fileSig, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fileSig{}, nil
		}
		return fileSig{}, errors.New("cannot stat username bindings")
	}
	return fileSig{ino: int64(st.Ino), size: st.Size, mod: st.Mtim.Nano(), exists: true}, nil
}

// readFile reads and parses the file without establishing durability. It
// returns the opened (and still open) file so the caller may fsync it.
func (b *Bindings) readFile() (map[string]int64, fileSig, *os.File, error) {
	fd, err := unix.Open(b.path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return map[string]int64{}, fileSig{}, nil, nil
		}
		return nil, fileSig{}, nil, errors.New("cannot open username bindings")
	}
	f := os.NewFile(uintptr(fd), b.path)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o077 != 0 || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, fileSig{}, nil, errors.New("username bindings must be an owned regular file with mode 0600")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBindingFile+1))
	if err != nil || len(raw) > maxBindingFile {
		f.Close()
		return nil, fileSig{}, nil, errors.New("invalid username bindings size")
	}
	m, err := parseBindings(raw)
	if err != nil {
		f.Close()
		return nil, fileSig{}, nil, err
	}
	return m, fileSig{ino: int64(st.Ino), size: st.Size, mod: st.Mtim.Nano(), exists: true}, f, nil
}

// lock takes the cross-process flock (shared or exclusive), waiting at most
// lockWait. Callers hold b.mu.
func (b *Bindings) lock(exclusive bool) (func(), error) {
	fd, err := unix.Open(b.lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, errors.New("cannot open username binding lock")
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	deadline := time.Now().Add(lockWait)
	for {
		err = unix.Flock(fd, how|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = unix.Close(fd)
			return nil, errors.New("cannot lock username bindings")
		}
		if time.Now().After(deadline) {
			_ = unix.Close(fd)
			return nil, errLockBusy
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// loadDurable makes b.byName a snapshot whose durability is established. The
// caller holds b.mu AND the flock. On any error the snapshot is dropped, so no
// authority can be derived from a stale or unproven view.
func (b *Bindings) loadDurable() error {
	sig, err := statSig(b.path)
	if err != nil {
		b.byName, b.verified = nil, fileSig{}
		return err
	}
	if !sig.exists {
		b.byName, b.verified = map[string]int64{}, fileSig{}
		return nil
	}
	if sig == b.verified && b.byName != nil {
		return nil
	}
	b.byName, b.verified = nil, fileSig{}
	m, rsig, f, err := b.readFile()
	if err != nil {
		return err
	}
	if f == nil { // vanished between stat and open: nothing to trust
		b.byName = map[string]int64{}
		return nil
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return errors.New("cannot make username bindings durable")
	}
	if err = syncDirHook(filepath.Dir(b.path)); err != nil {
		return errors.New("cannot make username bindings durable")
	}
	b.byName, b.verified = m, rsig
	return nil
}

// view runs fn on a durable snapshot under the shared lock (fail closed).
func (b *Bindings) view(fn func(map[string]int64)) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	release, err := b.lock(false)
	if err != nil {
		return false
	}
	defer release()
	if b.loadDurable() != nil {
		return false
	}
	fn(b.byName)
	return true
}

func parseBindings(raw []byte) (map[string]int64, error) {
	bad := errors.New("invalid username bindings")
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, bad
	}
	out := map[string]int64{}
	seenTop := map[string]bool{}
	for d.More() {
		k, err := d.Token()
		key, ok := k.(string)
		if err != nil || !ok || seenTop[key] {
			return nil, bad
		}
		seenTop[key] = true
		switch key {
		case "version":
			var v json.Number
			if d.Decode(&v) != nil || v.String() != "1" {
				return nil, bad
			}
		case "bindings":
			if t, err := d.Token(); err != nil || t != json.Delim('{') {
				return nil, bad
			}
			for d.More() {
				nk, err := d.Token()
				name, ok := nk.(string)
				if err != nil || !ok {
					return nil, bad
				}
				canon, cerr := CanonicalUsername(name)
				if cerr != nil || canon != name {
					return nil, bad
				}
				if _, dup := out[name]; dup {
					return nil, bad
				}
				var v json.Number
				if d.Decode(&v) != nil {
					return nil, bad
				}
				id, ierr := v.Int64()
				if ierr != nil || id <= 0 {
					return nil, bad
				}
				out[name] = id
			}
			if t, err := d.Token(); err != nil || t != json.Delim('}') {
				return nil, bad
			}
		default:
			return nil, bad
		}
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return nil, bad
	}
	if !seenTop["version"] || !seenTop["bindings"] || d.Decode(new(any)) != io.EOF {
		return nil, bad
	}
	return out, nil
}

// Claim binds the username slot to id if it is free, or succeeds if it is
// already bound to id. A different owner yields ErrBindingConflict; bindings are
// never reassigned. Success always means the pin (new or pre-existing) is
// durable: an existing file version is re-synced under the exclusive lock before
// it can be honoured, so a retry after a failed directory sync cannot succeed
// unless the sync now succeeds.
func (b *Bindings) Claim(username string, id int64) error {
	name, err := CanonicalUsername(username)
	if err != nil || id <= 0 {
		return errors.New("invalid username claim")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	release, err := b.lock(true)
	if err != nil {
		return err
	}
	defer release()
	if err = b.loadDurable(); err != nil {
		return err
	}
	if owner, ok := b.byName[name]; ok {
		if owner == id {
			return nil
		}
		return ErrBindingConflict
	}
	next := make(map[string]int64, len(b.byName)+1)
	for k, v := range b.byName {
		next[k] = v
	}
	next[name] = id
	// After the rename the new version is visible but unproven: drop trust until
	// loadDurable (here or in any other process, under the lock) syncs it.
	b.byName, b.verified = nil, fileSig{}
	if err = b.write(next); err != nil {
		return err
	}
	return b.loadDurable()
}

func (b *Bindings) write(m map[string]int64) error {
	enc, err := json.Marshal(struct {
		Version  int              `json:"version"`
		Bindings map[string]int64 `json:"bindings"`
	}{1, m})
	if err != nil {
		return errors.New("cannot encode username bindings")
	}
	dir := filepath.Dir(b.path)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.tmp", filepath.Base(b.path), os.Getpid()))
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot write username bindings")
	}
	renamed := false
	defer func() {
		if !renamed {
			f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(append(enc, '\n')); err != nil || f.Sync() != nil || f.Close() != nil {
		return errors.New("cannot write username bindings")
	}
	if err = os.Rename(tmp, b.path); err != nil {
		return errors.New("cannot replace username bindings")
	}
	renamed = true
	return nil
}

// syncDirHook is a test seam for fault injection of the directory fsync.
var syncDirHook = syncDir

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err = d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// Owner returns the numeric ID that owns the (canonicalized) username slot.
// Fail closed: any lock, read or durability problem yields no owner.
func (b *Bindings) Owner(username string) (int64, bool) {
	name, err := CanonicalUsername(username)
	if err != nil {
		return 0, false
	}
	var id int64
	var ok bool
	if !b.view(func(m map[string]int64) { id, ok = m[name] }) {
		return 0, false
	}
	return id, ok
}

// Names lists the username slots bound to id (none on any failure).
func (b *Bindings) Names(id int64) []string {
	var out []string
	if !b.view(func(m map[string]int64) {
		for n, owner := range m {
			if owner == id {
				out = append(out, n)
			}
		}
	}) {
		return nil
	}
	return out
}

// BoundTo reports whether id owns at least one slot that is also in the
// currently configured (canonical) allowlist.
func (b *Bindings) BoundTo(id int64, configured []string) bool {
	for _, n := range b.Names(id) {
		for _, c := range configured {
			if n == c {
				return true
			}
		}
	}
	return false
}
