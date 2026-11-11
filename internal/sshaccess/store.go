package sshaccess

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
)

var (
	ErrNotFound        = errors.New("sshaccess: no credentials for VM")
	ErrKeyNotGenerated = errors.New("sshaccess: owner key was supplied, not generated")
	ErrConflict        = errors.New("sshaccess: VM already has different credentials")
	ErrCorrupt         = errors.New("sshaccess: credential record unreadable")
)

const (
	masterKeyLen = 32
	recordMagic  = "vfc-ssh1"
)

var vmIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Public is the non-secret part of a VM's access. It is safe to persist in
// domain state, but the owner never needs to: it can be re-read from the Store.
type Public struct {
	OwnerPublic    string // canonical authorized_keys line for the VM owner
	OwnerGenerated bool   // true when the server generated (and holds) the owner key
	TerminalPublic string // canonical line for the server's per-VM ttyd identity
}

// record is the plaintext that is encrypted at rest.
type record struct {
	OwnerPublic     string   `json:"owner_public"`
	OwnerPrivate    string   `json:"owner_private,omitempty"` // empty when supplied
	TerminalPublic  string   `json:"terminal_public"`
	TerminalPrivate string   `json:"terminal_private"`
	Installation    *Receipt `json:"installation,omitempty"`
}

// Store keeps per-VM credentials encrypted (AES-256-GCM) in a directory, with
// the key held in a separate 0600 file. It is deliberately outside any DBOS
// workflow input, output, event or log: callers pass only VM ids and receive
// secrets only from OwnerPrivateKey / TerminalKeyFile.
type Store struct {
	dir  string
	aead cipher.AEAD
	mu   sync.Mutex
}

// GenerateMasterKeyFile creates a new random master key at path (0600,
// exclusive: never overwrites). Intended for provisioning scripts and tests.
func GenerateMasterKeyFile(path string) error {
	key := make([]byte, masterKeyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(key)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func readMasterKey(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("sshaccess: master key: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("sshaccess: master key must be a regular file with mode 0600 or stricter")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return nil, errors.New("sshaccess: master key must be owned by the service user")
	}
	if fi.Size() != masterKeyLen {
		return nil, fmt.Errorf("sshaccess: master key must be exactly %d raw bytes", masterKeyLen)
	}
	key := make([]byte, masterKeyLen)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, err
	}
	return key, nil
}

// OpenStore opens (creating dir with mode 0700 if needed) a credential store.
func OpenStore(dir, masterKeyFile string) (*Store, error) {
	key, err := readMasterKey(masterKeyFile)
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("sshaccess: credential directory must be a real directory with mode 0700")
	}
	return &Store{dir: dir, aead: aead}, nil
}

func checkID(id string) error {
	if !vmIDPattern.MatchString(id) {
		return fmt.Errorf("sshaccess: invalid VM id")
	}
	return nil
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".cred") }

func aad(id string) []byte { return []byte(recordMagic + ":" + id) }

func (s *Store) seal(id string, r record) ([]byte, error) {
	plain, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, s.aead.Seal(nil, nonce, plain, aad(id))...), nil
}

func (s *Store) load(id string) (record, error) {
	raw, err := os.ReadFile(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return record{}, ErrNotFound
	}
	if err != nil {
		return record{}, err
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns+s.aead.Overhead() {
		return record{}, ErrCorrupt
	}
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], aad(id))
	if err != nil {
		return record{}, ErrCorrupt
	}
	var r record
	if json.Unmarshal(plain, &r) != nil || r.OwnerPublic == "" || r.TerminalPrivate == "" {
		return record{}, ErrCorrupt
	}
	return r, nil
}

func (r record) public() Public {
	return Public{OwnerPublic: r.OwnerPublic, OwnerGenerated: r.OwnerPrivate != "", TerminalPublic: r.TerminalPublic}
}

// Ensure is idempotent per VM id. With suppliedPublicKey == "" it generates (once)
// an owner Ed25519 key; otherwise it validates and records the supplied key
// without any private half. A replay with the same intent returns the stored
// values; a different intent for an existing VM is ErrConflict, so a retried
// create can never silently regenerate or swap credentials. A terminal key
// is always generated separately.
func (s *Store) Ensure(vmID, suppliedPublicKey string) (Public, error) {
	if err := checkID(vmID); err != nil {
		return Public{}, err
	}
	var supplied string
	if suppliedPublicKey != "" {
		var err error
		if supplied, err = ParsePublicKey(suppliedPublicKey); err != nil {
			return Public{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, err := s.load(vmID); err == nil {
		if (supplied == "" && r.OwnerPrivate == "") || (supplied != "" && (r.OwnerPrivate != "" || r.OwnerPublic != supplied)) {
			return Public{}, ErrConflict
		}
		return r.public(), nil
	} else if !errors.Is(err, ErrNotFound) {
		return Public{}, err
	}
	var r record
	if supplied != "" {
		r.OwnerPublic = supplied
	} else {
		priv, pub, err := GenerateEd25519()
		if err != nil {
			return Public{}, err
		}
		r.OwnerPublic, r.OwnerPrivate = pub, string(priv)
	}
	tpriv, tpub, err := GenerateEd25519()
	if err != nil {
		return Public{}, err
	}
	r.TerminalPublic, r.TerminalPrivate = tpub, string(tpriv)
	blob, err := s.seal(vmID, r)
	if err != nil {
		return Public{}, err
	}
	if err := s.publish(vmID, blob); err != nil {
		return Public{}, err
	}
	return r.public(), nil
}

// publish writes durably and atomically, never replacing an existing record.
func (s *Store) publish(id string, blob []byte) error {
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), s.path(id)); err != nil { // fails if it exists
		return err
	}
	return syncDirPath(s.dir)
}

func syncDirPath(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Public returns the non-secret view of an existing record.
func (s *Store) Public(vmID string) (Public, error) {
	if err := checkID(vmID); err != nil {
		return Public{}, err
	}
	r, err := s.load(vmID)
	if err != nil {
		return Public{}, err
	}
	return r.public(), nil
}

// OwnerPrivateKey returns the generated owner private key (OpenSSH PEM). It is
// ErrKeyNotGenerated when the owner supplied their own public key. Callers must
// deliver it owner-only with Cache-Control: no-store and never log it.
func (s *Store) OwnerPrivateKey(vmID string) ([]byte, error) {
	if err := checkID(vmID); err != nil {
		return nil, err
	}
	r, err := s.load(vmID)
	if err != nil {
		return nil, err
	}
	if r.OwnerPrivate == "" {
		return nil, ErrKeyNotGenerated
	}
	return []byte(r.OwnerPrivate), nil
}

// TerminalKeyFile writes the VM's terminal (ttyd->sshd) private key to
// a unique leased file (dir created 0700, file 0600) and returns the absolute path
// plus a release func that removes it. ssh needs a file; keep it only as long as
// the terminal session lives and place dir on tmpfs/runtime storage.
func (s *Store) TerminalKeyFile(vmID, dir string) (string, func(), error) {
	if err := checkID(vmID); err != nil {
		return "", nil, err
	}
	r, err := s.load(vmID)
	if err != nil {
		return "", nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, err
	}
	if err := privateDirectory(abs); err != nil {
		return "", nil, err
	}
	tmp, err := os.CreateTemp(abs, vmID+"-*.key")
	if err != nil {
		return "", nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.WriteString(r.TerminalPrivate); err != nil {
		tmp.Close()
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		return "", nil, err
	}
	p := tmp.Name()
	ok = true
	var once sync.Once
	return p, func() { once.Do(func() { os.Remove(p) }) }, nil
}

// Delete removes a VM's credentials. Idempotent. Call only once the VM is
// proven deleted; retrieval is intended to work until then.
func (s *Store) Delete(vmID string) error {
	if err := checkID(vmID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(vmID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirPath(s.dir)
}
