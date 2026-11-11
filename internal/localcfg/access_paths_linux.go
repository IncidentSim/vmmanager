//go:build linux

package localcfg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Reject symlinks and replaceable key ancestry before the no-follow key open.
func validateMasterAncestors(path string) error {
	cur := "/"
	privateBarrier := false
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return errors.New("personal_ssh master key ancestry unavailable")
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || ((!privateBarrier || cur == filepath.Dir(path)) && fi.Mode().Perm()&0022 != 0) || (st.Uid != 0 && int(st.Uid) != os.Geteuid()) {
			return errors.New("personal_ssh master key ancestry must be real, trusted and not group/world writable")
		}
		if int(st.Uid) == os.Geteuid() && fi.Mode().Perm()&0077 == 0 {
			privateBarrier = true
		}
	}
	return nil
}
