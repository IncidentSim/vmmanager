package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

type profileConfig struct {
	Name    string `json:"name"`
	VCPU    uint32 `json:"vcpu"`
	MemMiB  uint32 `json:"mem_mib"`
	DiskMiB uint32 `json:"disk_mib"`
}

type bastionConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
}

type localConfig struct {
	WorkRoot   string `json:"work_root"`
	ImagesDir  string `json:"images_dir"`
	RunDir     string `json:"run_dir"`
	MaxVMs     int    `json:"max_vms"`
	Helper     string `json:"helper"`
	SSHUser    string `json:"ssh_user"`
	MinFreeMiB uint64 `json:"min_free_mib"`
	// StorageMount is the bounded storage mount point; required so a missing
	// mount can never fall back to the root filesystem.
	StorageMount string `json:"storage_mount"`
	// Aggregate admission; keep within the installed cgroup slice budget.
	MaxTotalMemMiB  uint32          `json:"max_total_mem_mib"`
	MaxTotalVCPU    uint32          `json:"max_total_vcpu"`
	UnitMemLimitMiB uint32          `json:"unit_mem_limit_mib"`
	VMMOverheadMiB  uint32          `json:"vmm_overhead_mib"`
	GuestDNS        []string        `json:"guest_dns,omitempty"`
	Profiles        []profileConfig `json:"profiles"`
	Bastion         *bastionConfig  `json:"bastion,omitempty"`
}

type remoteConfig struct {
	Addr       string `json:"addr"`
	ServerName string `json:"server_name"`
	CA         string `json:"ca"`
	Cert       string `json:"cert"`
	Key        string `json:"key"`
}

type backendConfig struct {
	Kind   string        `json:"kind"` // "local" or "remote"
	Local  *localConfig  `json:"local,omitempty"`
	Remote *remoteConfig `json:"remote,omitempty"`
}

type listenConfig struct {
	Listen string `json:"listen"`
	CA     string `json:"ca"`   // CA that signs ALLOWED CLIENT certificates for this listener
	Cert   string `json:"cert"` // this server's certificate
	Key    string `json:"key"`
}

// config deliberately has no field for plaintext, skipping verification or
// exposing a raw Firecracker socket.
type config struct {
	Backend      backendConfig `json:"backend"`
	ServeBackend *listenConfig `json:"serve_backend,omitempty"` // default off: more privileged
	ServeFacade  *listenConfig `json:"serve_facade,omitempty"`
}

func parseConfig(data []byte) (*config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, c.validate()
}

func (l *listenConfig) validate(name string) error {
	if l.Listen == "" || l.CA == "" || l.Cert == "" || l.Key == "" {
		return fmt.Errorf("%s: listen, ca, cert and key are all required (mutual TLS is mandatory)", name)
	}
	return nil
}

func (c *config) validate() error {
	if c.ServeBackend == nil && c.ServeFacade == nil {
		return errors.New("nothing to serve: set serve_backend and/or serve_facade")
	}
	switch c.Backend.Kind {
	case "local":
		l := c.Backend.Local
		if l == nil || c.Backend.Remote != nil {
			return errors.New("backend.kind local requires backend.local only")
		}
		if l.Helper == "" || !filepath.IsAbs(l.Helper) {
			return errors.New("backend.local.helper must be an absolute path to the installed launcher helper")
		}
		if l.WorkRoot == "" || l.ImagesDir == "" || l.RunDir == "" {
			return errors.New("backend.local work_root, images_dir and run_dir are required")
		}
		if l.StorageMount == "" {
			return errors.New("backend.local.storage_mount is required (bounded storage mount guard)")
		}
		if l.MaxTotalMemMiB == 0 || l.MaxTotalVCPU == 0 || l.UnitMemLimitMiB == 0 {
			return errors.New("backend.local max_total_mem_mib, max_total_vcpu and unit_mem_limit_mib are required admission policy")
		}
	case "remote":
		r := c.Backend.Remote
		if r == nil || c.Backend.Local != nil {
			return errors.New("backend.kind remote requires backend.remote only")
		}
		if r.Addr == "" || r.ServerName == "" || r.CA == "" || r.Cert == "" || r.Key == "" {
			return errors.New("backend.remote requires addr, server_name (hostname verification), ca, cert and key")
		}
		if c.ServeBackend != nil {
			return errors.New("serve_backend would re-export a remote backend; only a local backend may be served raw")
		}
	default:
		return fmt.Errorf("backend.kind must be local or remote, got %q", c.Backend.Kind)
	}
	if c.ServeBackend != nil {
		if err := c.ServeBackend.validate("serve_backend"); err != nil {
			return err
		}
	}
	if c.ServeFacade != nil {
		if err := c.ServeFacade.validate("serve_facade"); err != nil {
			return err
		}
	}
	return nil
}
