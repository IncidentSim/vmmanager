//go:build linux

package vmhelper

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockSlot excludes independent socket-activated helper processes. Never unlink
// these files: replacing an inode would split the exclusion domain. The parent
// is root-owned in production and not writable by the unprivileged client.
func (h SystemdHandler) lockSlot(slot int) (func(), error) {
	dir := h.LockDir
	if dir == "" {
		dir = "/run/firecracker-vms"
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open helper lock directory: %w", err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		return nil, fmt.Errorf("unsafe helper lock directory: uid=%d mode=%o (want owner %d, no group/other writes)", st.Uid, st.Mode, os.Geteuid())
	}
	lock, err := unix.Openat(fd, fmt.Sprintf(".helper-slot-%d.lock", slot), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open slot lock: %w", err)
	}
	if err := unix.Fstat(lock, &st); err != nil {
		unix.Close(lock)
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 || st.Nlink != 1 {
		unix.Close(lock)
		return nil, fmt.Errorf("unsafe helper slot lock")
	}
	if err := unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(lock)
		return nil, fmt.Errorf("slot %d start transaction busy: %w", slot, err)
	}
	return func() { unix.Close(lock) }, nil
}
