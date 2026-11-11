// Package localcfg is the single definition of the private JSON policy for the
// single-host local backend (formerly vmd's backend.local object) and the
// assembly of that policy into a vmlib/local.Config. It carries no transport.
// Helper paths, profiles and host addresses come only from this operator-owned
// file, never from client input. The test-only trusted UID/ancestor seams are
// deliberately not representable in JSON.
package localcfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

type Profile struct {
	Name      string          `json:"name"`
	VCPU      uint32          `json:"vcpu"`
	MemMiB    uint32          `json:"mem_mib"`
	DiskMiB   uint32          `json:"disk_mib"`
	CPULimit  json.RawMessage `json:"cpu_limit,omitempty"`
	MemoryMiB *uint32         `json:"memory_mib,omitempty"`
}

type Bastion struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
}

// JSONConfig mirrors the field set and names of the former vmd
// backend.local object exactly.
type JSONConfig struct {
	// PublicSSH is an optional assertion against root PoolConf publication.
	// Omission inherits the pool; explicit hosts must match its literal IPv4.
	PublicSSH   *vmlib.PublicSSHConfig `json:"public_ssh,omitempty"`
	PersonalSSH *PersonalSSHConfig     `json:"personal_ssh,omitempty"`
	WorkRoot    string                 `json:"work_root"`
	ImagesDir   string                 `json:"images_dir"`
	RunDir      string                 `json:"run_dir"`
	MaxVMs      int                    `json:"max_vms"`
	Helper      string                 `json:"helper"`
	SSHUser     string                 `json:"ssh_user"`
	MinFreeMiB  uint64                 `json:"min_free_mib"`
	// StorageMount is the bounded storage mount point; required so a missing
	// mount can never fall back to the root filesystem.
	StorageMount string `json:"storage_mount"`
	// Aggregate admission; keep within the installed cgroup slice budget.
	MaxTotalMemMiB   uint32          `json:"max_total_mem_mib"`
	MaxTotalVCPU     uint32          `json:"max_total_vcpu"`
	MaxTotalCPULimit json.RawMessage `json:"max_total_cpu_limit,omitempty"`
	DefaultProfile   string          `json:"default_profile,omitempty"`
	UnitMemLimitMiB  uint32          `json:"unit_mem_limit_mib"`
	VMMOverheadMiB   uint32          `json:"vmm_overhead_mib"`
	GuestDNS         []string        `json:"guest_dns,omitempty"`
	// GuestNetworkProvisioning opts into private-copy boot provisioning.
	GuestNetworkProvisioning string `json:"guest_network_provisioning,omitempty"`
	// GuestPrefix is the operator-chosen guest pool "A.B" (slot N = A.B.N.0/30);
	// PoolConf is the shared root-owned pool file that must agree with it.
	GuestPrefix string    `json:"guest_prefix"`
	PoolConf    string    `json:"pool_conf"`
	Profiles    []Profile `json:"profiles"`
	Bastion     *Bastion  `json:"bastion,omitempty"`
}

// Parse strictly decodes a standalone JSON object (unknown fields and trailing
// data fail) and validates it.
func Parse(data []byte) (*JSONConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c JSONConfig
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("backend config: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("backend config: trailing data")
	}
	return &c, c.Validate()
}

// Validate enforces the operator-policy requirements formerly checked by vmd.
func (l *JSONConfig) Validate() error {
	if l.GuestNetworkProvisioning != "" {
		if l.GuestNetworkProvisioning != "ubuntu-fcnet" || len(l.GuestDNS) == 0 {
			return errors.New("guest_network_provisioning must be ubuntu-fcnet with nonempty guest_dns")
		}
		for _, dns := range l.GuestDNS {
			if net.ParseIP(dns) == nil {
				return errors.New("guest_dns must contain literal IP addresses for provisioning")
			}
		}
	}
	if err := l.PublicSSH.ValidateSlots(l.MaxVMs); err != nil {
		return err
	}
	if err := l.validatePersonalSSH(); err != nil {
		return err
	}
	if l.Helper == "" || !filepath.IsAbs(l.Helper) {
		return errors.New("backend.helper must be an absolute path to the installed launcher helper")
	}
	if l.WorkRoot == "" || l.ImagesDir == "" || l.RunDir == "" {
		return errors.New("backend work_root, images_dir and run_dir are required")
	}
	if l.StorageMount == "" {
		return errors.New("backend.storage_mount is required (bounded storage mount guard)")
	}
	if l.GuestPrefix == "" || l.PoolConf == "" {
		return errors.New("backend guest_prefix and pool_conf are required (operator-chosen collision-free pool)")
	}
	if l.MaxTotalMemMiB == 0 || l.MaxTotalVCPU == 0 || l.UnitMemLimitMiB == 0 {
		return errors.New("backend max_total_mem_mib, max_total_vcpu and unit_mem_limit_mib are required admission policy")
	}
	if len(l.MaxTotalCPULimit) != 0 {
		if _, err := vmlib.ParseCPULimit(string(l.MaxTotalCPULimit)); err != nil {
			return fmt.Errorf("max_total_cpu_limit: %w", err)
		}
	}
	seen := map[string]bool{}
	for _, p := range l.Profiles {
		effective, err := p.effective()
		if err != nil {
			return err
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate profile %q", p.Name)
		}
		seen[p.Name] = true
		if err := effective.Validate(); err != nil {
			return err
		}
	}
	if len(l.Profiles) == 0 {
		return errors.New("profiles are required")
	}
	if l.DefaultProfile != "" && !seen[l.DefaultProfile] {
		return fmt.Errorf("unknown default_profile %q", l.DefaultProfile)
	}
	return nil
}

func (p Profile) effective() (vmlib.Profile, error) {
	mem := p.MemMiB
	if p.MemoryMiB != nil {
		if p.MemMiB != 0 {
			return vmlib.Profile{}, errors.New("use only memory_mib or legacy mem_mib")
		}
		mem = *p.MemoryMiB
	}
	cpu := uint32(0)
	if len(p.CPULimit) != 0 {
		var err error
		cpu, err = vmlib.ParseCPULimit(string(p.CPULimit))
		if err != nil {
			return vmlib.Profile{}, err
		}
	}
	if cpu == 0 && p.VCPU <= vmlib.MaxVCPU {
		cpu = p.VCPU * vmlib.CPUMilli
	}
	return vmlib.Profile{Name: p.Name, VCPU: p.VCPU, CPULimitMilli: cpu, MemMiB: mem, DiskMiB: p.DiskMiB}, nil
}
