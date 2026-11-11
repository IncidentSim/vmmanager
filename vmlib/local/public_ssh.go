//go:build linux

package local

import (
	"fmt"
	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

// ResolvePublicSSH performs read-only publication preflight before composition
// opens credential stores or backend roots. A configured root pool is the only
// publication authority; DNS is deliberately not consulted.
func ResolvePublicSSH(poolConf string, policy *vmlib.PublicSSHConfig, slots int) (*vmlib.PublicSSHConfig, error) {
	if poolConf == "" {
		if err := policy.ValidateSlots(slots); err != nil {
			return nil, err
		}
		if policy == nil {
			return nil, nil
		}
		copy := *policy
		return &copy, nil
	}
	pool, err := vmhelper.ReadPool(poolConf)
	if err != nil {
		return nil, fmt.Errorf("pool configuration: %w", err)
	}
	return publicSSHFromPool(pool, policy, slots)
}

func publicSSHFromPool(pool vmhelper.Pool, policy *vmlib.PublicSSHConfig, slots int) (*vmlib.PublicSSHConfig, error) {
	if policy != nil && (pool.PublicIPv4 == "" || policy.Host != pool.PublicIPv4 || policy.PortBase != pool.SSHPortBase) {
		return nil, fmt.Errorf("public_ssh must match root pool literal PUBLIC_IPV4 and SSH_PORT_BASE; publication may be disabled")
	}
	if err := policy.ValidateSlots(slots); err != nil {
		return nil, err
	}
	if pool.PublicIPv4 == "" {
		return nil, nil
	}
	resolved := &vmlib.PublicSSHConfig{Host: pool.PublicIPv4, PortBase: pool.SSHPortBase}
	if err := resolved.ValidateSlots(slots); err != nil {
		return nil, err
	}
	return resolved, nil
}
