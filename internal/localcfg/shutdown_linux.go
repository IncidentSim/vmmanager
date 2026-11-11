//go:build linux

package localcfg

import (
	"errors"
	"os"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh/knownhosts"
)

func validateShutdownKnownHosts(path string) error {
	invalid := errors.New("personal_ssh shutdown known hosts must be a readable, nonempty, valid trusted regular file with trusted ancestry")
	if validateMasterAncestors(path) != nil {
		return invalid
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0022 != 0 || fi.Size() == 0 || fi.Size() > 1<<20 {
		return invalid
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || (st.Uid != 0 && int(st.Uid) != os.Geteuid()) {
		return invalid
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return invalid
	}
	if _, err := knownhosts.New(path); err != nil {
		return invalid
	}
	return nil
}
