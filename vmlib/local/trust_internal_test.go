//go:build linux

package local

import (
	"testing"

	"golang.org/x/sys/unix"
)

func st(mode uint32, uid uint32) *unix.Stat_t { return &unix.Stat_t{Mode: mode, Uid: uid} }

// Owner checks cannot be exercised by chown in an unprivileged test, so the pure
// predicate is tested with synthetic stat data.
func TestCheckStatOwnerAndModeRules(t *testing.T) {
	const root = 0
	cases := []struct {
		name string
		st   *unix.Stat_t
		kind string
		ok   bool
	}{
		{"dir root 0755", st(unix.S_IFDIR|0o755, root), "dir", true},
		{"dir root 0750", st(unix.S_IFDIR|0o750, root), "dir", true},
		{"dir owned by client", st(unix.S_IFDIR|0o755, 1000), "dir", false},
		{"dir group writable", st(unix.S_IFDIR|0o775, root), "dir", false},
		{"dir world writable", st(unix.S_IFDIR|0o757, root), "dir", false},
		{"dir is a symlink", st(unix.S_IFLNK|0o777, root), "dir", false},
		{"dir is a file", st(unix.S_IFREG|0o644, root), "dir", false},
		{"receipt root 0640", st(unix.S_IFREG|0o640, root), "receipt", true},
		{"receipt owned by client", st(unix.S_IFREG|0o640, 1000), "receipt", false},
		{"receipt group writable", st(unix.S_IFREG|0o660, root), "receipt", false},
		{"receipt is a dir", st(unix.S_IFDIR|0o755, root), "receipt", false},
		{"lock root 0660", st(unix.S_IFREG|0o660, root), "lock", true},
		{"lock world writable", st(unix.S_IFREG|0o662, root), "lock", false},
		{"lock owned by client", st(unix.S_IFREG|0o660, 1000), "lock", false},
	}
	for _, c := range cases {
		if err := checkStat(c.st, root, c.kind); (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", c.name, err, c.ok)
		}
	}
}
