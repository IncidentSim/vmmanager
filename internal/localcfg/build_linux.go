//go:build linux

package localcfg

import (
	"path/filepath"

	"github.com/petruekhin/vibe-firecracker/internal/sshaccess"
	"github.com/petruekhin/vibe-firecracker/vmlib"
	"github.com/petruekhin/vibe-firecracker/vmlib/local"
)

// Seams lets tests replace the host seams. Production passes the zero value:
// a real helper launcher and the Firecracker SDK VMM, root-owned runtime tree.
type Seams struct {
	Launcher local.Launcher
	VMM      local.VMM
	// TrustedOwnerUID and TrustedAncestorRoot are test seams for unprivileged
	// runs; they are never read from configuration.
	TrustedOwnerUID     *uint32
	TrustedAncestorRoot string
}

// Assemble builds the local.Config (including the runtime Launcher and VMM
// dependencies) from the validated policy.
func (l *JSONConfig) Assemble(s Seams) (local.Config, error) {
	publicSSH, err := local.ResolvePublicSSH(l.PoolConf, l.PublicSSH, l.MaxVMs)
	if err != nil {
		return local.Config{}, err
	}
	access, err := l.openAccess()
	if err != nil {
		return local.Config{}, err
	}
	launcher, vmm := s.Launcher, s.VMM
	if launcher == nil {
		h, err := local.NewHelperLauncher(l.Helper)
		if err != nil {
			return local.Config{}, err
		}
		launcher = h
	}
	if vmm == nil {
		vmm = local.SDKVMM{RunDir: l.RunDir}
	}
	var profiles []vmlib.Profile
	for _, p := range l.Profiles {
		effective, err := p.effective()
		if err != nil {
			return local.Config{}, err
		}
		profiles = append(profiles, effective)
	}
	var cpu uint32
	if len(l.MaxTotalCPULimit) != 0 {
		var err error
		cpu, err = vmlib.ParseCPULimit(string(l.MaxTotalCPULimit))
		if err != nil {
			return local.Config{}, err
		}
	}
	var bastion *vmlib.Bastion
	if l.Bastion != nil {
		bastion = &vmlib.Bastion{Host: l.Bastion.Host, Port: l.Bastion.Port, User: l.Bastion.User}
	}
	var network local.OfflineGuestNetwork
	if l.GuestNetworkProvisioning == "ubuntu-fcnet" {
		network = sshaccess.UbuntuFCNetProvisioner{}
	}
	var shutdown local.CooperativeShutdown
	if a, ok := access.(*accessHooks); ok && l.PersonalSSH.ShutdownKnownHostsFile != "" {
		shutdown = sshaccess.SSHShutdown{Credentials: a.store,
			RuntimeDir:     filepath.Join(l.PersonalSSH.StoreDir, "shutdown-leases"),
			KnownHostsFile: l.PersonalSSH.ShutdownKnownHostsFile}
	}
	return local.Config{CooperativeShutdown: shutdown, Access: access, WorkRoot: l.WorkRoot, ImagesDir: l.ImagesDir, RunDir: l.RunDir, MaxVMs: l.MaxVMs,
		MaxTotalCPULimitMilli: cpu, DefaultProfile: l.DefaultProfile,
		PublicSSH: publicSSH, Profiles: profiles, SSHUser: l.SSHUser, Bastion: bastion, Launcher: launcher, VMM: vmm, MinFreeMiB: l.MinFreeMiB,
		StorageMount: l.StorageMount, MaxTotalMemMiB: l.MaxTotalMemMiB, MaxTotalVCPU: l.MaxTotalVCPU,
		UnitMemLimitMiB: l.UnitMemLimitMiB, VMMOverheadMiB: l.VMMOverheadMiB, GuestDNS: l.GuestDNS,
		GuestNetwork: network, GuestPrefix: l.GuestPrefix, PoolConf: l.PoolConf,
		TrustedOwnerUID: s.TrustedOwnerUID, TrustedAncestorRoot: s.TrustedAncestorRoot}, nil
}

// Open takes the exclusive lifetime locks on the configured roots. Close of the
// returned backend releases the locks only; it never stops or deletes a VM.
func Open(l *JSONConfig, s Seams) (*local.Backend, error) {
	b, _, err := BuildWithAccess(l, s)
	return b, err
}
