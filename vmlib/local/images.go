//go:build linux

package local

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/petruekhin/vibe-firecracker/vmlib"
	"golang.org/x/sys/unix"
)

// DeleteImage serializes against Prepare admission and publication reservations.
// Only a regular image carrying a regular managed-publication marker is eligible.
// Open directory descriptors without following symlinks; unlinkat never follows
// the image itself. Operators must not concurrently mutate backend-owned roots.
func (b *Backend) DeleteImage(ctx context.Context, id string) error {
	leave, err := b.enter()
	if err != nil {
		return err
	}
	defer leave()
	if err := vmlib.ValidateID("image", id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.corrupt) > 0 {
		return fmt.Errorf("%w: unreadable workspace references", vmlib.ErrNotStopped)
	}
	if _, busy := b.publishing[id]; busy {
		return fmt.Errorf("%w: publication in flight", vmlib.ErrNotStopped)
	}
	for _, e := range b.entries {
		if e.m.ImageID == id {
			return fmt.Errorf("%w: image referenced by a VM", vmlib.ErrNotStopped)
		}
	}
	root, err := unix.Open(b.cfg.ImagesDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	var st unix.Stat_t
	err = unix.Fstatat(root, id+".ext4", &st, unix.AT_SYMLINK_NOFOLLOW)
	missing := errors.Is(err, unix.ENOENT)
	if err != nil && !missing {
		return err
	}
	if !missing && st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: image is not regular", vmlib.ErrInvalidArgument)
	}
	catalog, err := unix.Openat(root, ".committed", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		if missing {
			return nil
		}
		return fmt.Errorf("%w: immutable base image", vmlib.ErrNotStopped)
	}
	if err != nil {
		return fmt.Errorf("%w: unsafe managed catalog: %v", vmlib.ErrInvalidArgument, err)
	}
	defer unix.Close(catalog)
	err = unix.Fstatat(catalog, id, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		if missing {
			return nil
		}
		return fmt.Errorf("%w: immutable base image", vmlib.ErrNotStopped)
	}
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: unsafe managed marker", vmlib.ErrInvalidArgument)
	}
	// An orphan marker can be an interrupted publication. Do not erase its evidence.
	if missing {
		return fmt.Errorf("%w: incomplete publication or deletion requires reconciliation", vmlib.ErrNotStopped)
	}
	if err := unix.Unlinkat(root, id+".ext4", 0); err != nil {
		return err
	}
	if err := syncDir(b.cfg.ImagesDir); err != nil {
		return err
	}
	if err := unix.Unlinkat(catalog, id, 0); err != nil {
		return err
	}
	return syncDir(filepath.Join(b.cfg.ImagesDir, ".committed"))
}
