//go:build linux

package localcfg

import (
	"errors"
	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
	"os/exec"
)

type accessHooks struct {
	*sshaccess.Provisioner
	store *sshaccess.Store
}

func (a *accessHooks) Delete(id string) error { return a.store.Delete(id) }

func (l *JSONConfig) openAccess() (local.OfflineAccess, error) {
	if err := l.validatePersonalSSH(); err != nil {
		return nil, err
	}
	if l.PersonalSSH == nil || !l.PersonalSSH.Enabled {
		return nil, nil
	}
	c := l.PersonalSSH
	if c.ShutdownKnownHostsFile != "" {
		if err := validateShutdownKnownHosts(c.ShutdownKnownHostsFile); err != nil {
			return nil, err
		}
	}
	debugfs, err := exec.LookPath("debugfs")
	if err != nil {
		return nil, errors.New("personal_ssh requires debugfs in service PATH")
	}
	if err := validateMasterAncestors(c.MasterKeyFile); err != nil {
		return nil, err
	}
	store, err := sshaccess.OpenStore(c.StoreDir, c.MasterKeyFile)
	if err != nil {
		return nil, err
	}
	return &accessHooks{Provisioner: &sshaccess.Provisioner{Store: store, User: l.SSHUser, Debugfs: debugfs}, store: store}, nil
}

// BuildWithAccess constructs the real backend and returns its shared credential
// provider for owner-only HTTP retrieval and dedicated terminal key leases. It
// returns nil credentials when personal access is disabled. Never serialize the
// returned provider or any private-key result into workflow state.
func BuildWithAccess(l *JSONConfig, s Seams) (*local.Backend, sshaccess.Credentials, error) {
	cfg, err := l.Assemble(s)
	if err != nil {
		return nil, nil, err
	}
	b, err := local.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	if a, ok := cfg.Access.(*accessHooks); ok {
		return b, a.store, nil
	}
	return b, nil, nil
}
