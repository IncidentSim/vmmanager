package localcfg

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

// PersonalSSHConfig contains paths only, never key material. Enabling access is
// explicit; startup opens and validates the existing master key, never creates it.
type PersonalSSHConfig struct {
	Enabled       bool   `json:"enabled"`
	StoreDir      string `json:"store_dir"`
	MasterKeyFile string `json:"master_key_file"`
	// Nonempty opts into cooperative shutdown; no implicit host trust.
	ShutdownKnownHostsFile string `json:"shutdown_known_hosts_file,omitempty"`
}

func (l *JSONConfig) validatePersonalSSH() error {
	c := l.PersonalSSH
	if c == nil {
		return nil
	}
	if !c.Enabled {
		if c.StoreDir != "" || c.MasterKeyFile != "" || c.ShutdownKnownHostsFile != "" {
			return errors.New("personal_ssh paths require explicit enabled=true")
		}
		return nil
	}
	for _, p := range []string{c.StoreDir, c.MasterKeyFile} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, "\r\n\x00") {
			return errors.New("personal_ssh paths must be clean absolute paths")
		}
	}
	if p := c.ShutdownKnownHostsFile; p != "" {
		// OpenSSH expands tokens and splits filename lists in this option.
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, " 	\r\n\x00%~\"'\\") {
			return errors.New("personal_ssh shutdown known hosts must be an explicit clean absolute path without SSH tokens")
		}
	}
	if c.StoreDir == c.MasterKeyFile || strings.HasPrefix(c.MasterKeyFile, c.StoreDir+string(filepath.Separator)) {
		return errors.New("personal_ssh master key must be outside credential store")
	}
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(l.SSHUser) {
		return errors.New("personal_ssh requires a supported ssh_user")
	}
	return nil
}
