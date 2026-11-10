//go:build linux

package local

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Trust model for the launcher runtime tree (default RunDir /run/firecracker-vms):
// it is created and owned by root and is NEVER writable by the unprivileged
// backend. Exit receipts are the only proof that a VMM is gone, so a receipt is
// believed only if its whole path is root-owned and not writable by group or
// others, and no component is a symlink. The one writable thing the backend needs
// there is the run lock: the operator precreates `.backend.lock` (root:<group>
// 0660, see deployment/multivm/firecracker-vms.tmpfiles.conf) and the backend only
// opens it, it never creates it.

// trustedUID is the owner the runtime tree must have: root. Config.TrustedOwnerUID
// exists solely so unprivileged tests can use their own uid; vmd never sets it.
func (c *Config) trustedUID() uint32 {
	if c.TrustedOwnerUID != nil {
		return *c.TrustedOwnerUID
	}
	return 0
}

// checkStat validates one object. kind is "dir" (no group/other write), "receipt"
// (regular, no group/other write) or "lock" (regular, no other write; group write
// is the point of the precreated lock).
func checkStat(st *unix.Stat_t, uid uint32, kind string) error {
	typ := st.Mode & unix.S_IFMT
	switch kind {
	case "dir":
		if typ != unix.S_IFDIR {
			return errors.New("not a directory (symlinks are refused)")
		}
	case "receipt", "lock":
		if typ != unix.S_IFREG {
			return errors.New("not a regular file (symlinks are refused)")
		}
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	if st.Uid != uid {
		return fmt.Errorf("owned by uid %d, not the trusted uid %d", st.Uid, uid)
	}
	mask := uint32(0o022)
	if kind == "lock" {
		mask = 0o002
	}
	if st.Mode&mask != 0 {
		return fmt.Errorf("mode %04o is writable by an untrusted principal", st.Mode&0o7777)
	}
	return nil
}

// validateRunHierarchy checks the canonical runtime directory, every ancestor and
// every existing slot directory. It performs no writes.
func validateRunHierarchy(run string, uid uint32, floor string) error {
	var st unix.Stat_t
	if err := unix.Lstat(run, &st); err != nil {
		return err
	}
	if err := checkStat(&st, uid, "dir"); err != nil {
		return fmt.Errorf("runtime directory %s: %w", run, err)
	}
	for dir := filepath.Dir(run); ; dir = filepath.Dir(dir) {
		if err := unix.Lstat(dir, &st); err != nil {
			return err
		}
		if st.Uid != 0 && st.Uid != uid {
			return fmt.Errorf("ancestor %s is owned by uid %d", dir, st.Uid)
		}
		// A sticky directory (e.g. /tmp) does not let others replace our entries.
		if st.Mode&0o022 != 0 && st.Mode&unix.S_ISVTX == 0 {
			return fmt.Errorf("ancestor %s is writable by group or others", dir)
		}
		if dir == "/" || dir == filepath.Dir(dir) || (floor != "" && dir == floor) {
			break
		}
	}
	entries, err := os.ReadDir(run)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "slot-") {
			continue
		}
		if err := unix.Lstat(filepath.Join(run, e.Name()), &st); err != nil {
			return err
		}
		if err := checkStat(&st, uid, "dir"); err != nil {
			return fmt.Errorf("slot directory %s: %w", e.Name(), err)
		}
	}
	return nil
}

// openRunLock opens the PRECREATED lock file (never creates it) and takes the
// exclusive lock.
func openRunLock(path string, uid uint32) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("run lock %s must be precreated by the operator (root-owned, group-writable, not a symlink): %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	if err := checkStat(&st, uid, "lock"); err != nil {
		f.Close()
		return nil, fmt.Errorf("run lock %s: %w", path, err)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another backend owns %s: %w", filepath.Dir(path), err)
	}
	return f, nil
}

// secureReceipt reads <run>/slot-N/<inv>.exit.json through descriptors opened
// with O_NOFOLLOW so a symlink or a swapped directory can never redirect it.
// (data, present, err): a missing slot directory or file is "not present"; any
// trust violation is an ERROR, never "absent", so callers fail closed.
func (b *Backend) secureReceipt(slot int, inv string) ([]byte, bool, error) {
	uid := b.cfg.trustedUID()
	slotFD, err := unix.Openat(b.runFD, fmt.Sprintf("slot-%d", slot), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("untrusted runtime slot directory: %w", err)
	}
	defer unix.Close(slotFD)
	var st unix.Stat_t
	if err := unix.Fstat(slotFD, &st); err != nil {
		return nil, false, err
	}
	if err := checkStat(&st, uid, "dir"); err != nil {
		return nil, false, fmt.Errorf("slot-%d: %w", slot, err)
	}
	name := inv + ".exit.json"
	fd, err := unix.Openat(slotFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("untrusted receipt %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, false, err
	}
	if err := checkStat(&st, uid, "receipt"); err != nil {
		return nil, false, fmt.Errorf("receipt %s: %w", name, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	return data, true, err
}
