package sshaccess

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Receipt is trusted host-side installation ownership metadata. It contains no
// private keys. Persist it before editing the disk; never accept it from a guest.
type Receipt struct {
	VMID           string
	User           string
	Path           string
	Lines          []string
	Created        bool
	Separator      bool
	OriginalLength int
	OriginalPrefix string
}

const receiptPath = "/etc/vibe-fc-ssh-access"

// privateDirectory refuses symlinks anywhere, foreign-owned or writable
// ancestors, and a non-private final directory. Concurrent mutation by the
// service UID is outside this filesystem trust boundary.
func privateDirectory(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	cur := "/"
	privateBarrier := false
	for _, part := range strings.Split(strings.TrimPrefix(abs, "/"), "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(cur, 0700); err != nil {
				return err
			}
			fi, err = os.Lstat(cur)
		}
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || (int(st.Uid) != os.Geteuid() && st.Uid != 0) || (!privateBarrier && fi.Mode().Perm()&0022 != 0) {
			return errors.New("sshaccess: unsafe directory ancestor")
		}
		if int(st.Uid) == os.Geteuid() && fi.Mode().Perm()&0077 == 0 {
			privateBarrier = true
		}
		if cur == abs && (int(st.Uid) != os.Geteuid() || fi.Mode().Perm()&0077 != 0) {
			return errors.New("sshaccess: directory must be private and service-owned")
		}
	}
	return nil
}

func (s *Store) saveReceipt(id string, receipt *Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.load(id)
	if err != nil {
		return err
	}
	r.Installation = receipt
	blob, err := s.seal(id, r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(blob); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.path(id)); err != nil {
		return err
	}
	return syncDirPath(s.dir)
}

// ScrubVM is the publication seam. It uses durable trusted host metadata even
// if a guest removed its discovery marker. A changed/missing account fails closed.
func (p *Provisioner) ScrubVM(ctx context.Context, vmID, disk string) error {
	if p.Store == nil {
		return errors.New("sshaccess: no credential store")
	}
	if err := checkID(vmID); err != nil {
		return err
	}
	r, err := p.Store.load(vmID)
	if err != nil {
		return err
	}
	if r.Installation == nil {
		return ErrUnsupportedImage
	}
	return p.scrubReceipt(ctx, disk, r.Installation)
}

// Scrub discovers a VM ID and resolves its trusted host receipt. Publication callers should
// use ScrubVM: a guest may remove the discovery marker.
func (p *Provisioner) Scrub(ctx context.Context, disk string) error {
	g := d(p)
	if _, _, _, err := g.account(ctx, disk, p.User); err != nil {
		return err
	}
	fi, err := g.stat(ctx, disk, receiptPath)
	if err != nil {
		return err
	}
	if !fi.exists {
		return nil
	}
	if fi.typ != "regular" || p.Store == nil {
		return ErrUnsupportedImage
	}
	data, err := g.readFile(ctx, disk, receiptPath)
	if err != nil {
		return err
	}
	var id string
	if json.Unmarshal(data, &id) != nil || checkID(id) != nil {
		return ErrUnsupportedImage
	}
	return p.ScrubVM(ctx, id, disk)
}
