//go:build linux

package localcfg

import (
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
		profiles = append(profiles, vmlib.Profile{Name: p.Name, VCPU: p.VCPU, MemMiB: p.MemMiB, DiskMiB: p.DiskMiB})
	}
	var bastion *vmlib.Bastion
	if l.Bastion != nil {
		bastion = &vmlib.Bastion{Host: l.Bastion.Host, Port: l.Bastion.Port, User: l.Bastion.User}
	}
	return local.Config{WorkRoot: l.WorkRoot, ImagesDir: l.ImagesDir, RunDir: l.RunDir, MaxVMs: l.MaxVMs,
		Profiles: profiles, SSHUser: l.SSHUser, Bastion: bastion, Launcher: launcher, VMM: vmm, MinFreeMiB: l.MinFreeMiB,
		StorageMount: l.StorageMount, MaxTotalMemMiB: l.MaxTotalMemMiB, MaxTotalVCPU: l.MaxTotalVCPU,
		UnitMemLimitMiB: l.UnitMemLimitMiB, VMMOverheadMiB: l.VMMOverheadMiB, GuestDNS: l.GuestDNS,
		GuestPrefix: l.GuestPrefix, PoolConf: l.PoolConf,
		TrustedOwnerUID: s.TrustedOwnerUID, TrustedAncestorRoot: s.TrustedAncestorRoot}, nil
}

// Open takes the exclusive lifetime locks on the configured roots. Close of the
// returned backend releases the locks only; it never stops or deletes a VM.
func Open(l *JSONConfig, s Seams) (*local.Backend, error) {
	c, err := l.Assemble(s)
	if err != nil {
		return nil, err
	}
	return local.New(c)
}
