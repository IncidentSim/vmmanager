package vmlib

import (
	"fmt"
	"net"
	"strings"
)

// SSHEndpoint is backend-derived metadata, never a client forwarding request.
type SSHEndpoint struct {
	Host string
	Port int
}

// PublicSSHConfig is operator-only advertisement policy. The operator must
// separately install IPv4 DNAT of PortBase+slot to the persisted slot guest:22.
// A nil configuration disables advertisement and preserves legacy bastion use.
type PublicSSHConfig struct {
	Host     string `json:"host"`
	PortBase int    `json:"port_base"`
}

// ValidateSlots bounds the entire slot range without overflowing integer addition.
// Host accepts a literal IPv4 or conservative DNS name; URLs, ports, options and
// IPv6 are not supported by the IPv4 forwarding deployment.
func (p *PublicSSHConfig) ValidateSlots(slots int) error {
	if p == nil {
		return nil
	}
	if slots < 1 || slots > 65535 || p.PortBase < 1 || p.PortBase > 65536-slots {
		return fmt.Errorf("public_ssh.port_base must cover all %d slots within 1..65535", slots)
	}
	h := p.Host
	if ip := net.ParseIP(h); ip != nil {
		if ip.To4() != nil && !strings.Contains(h, ":") {
			return nil
		}
		return fmt.Errorf("public_ssh.host must be IPv4 or a DNS name")
	}
	if h == "" || len(h) > 253 {
		return fmt.Errorf("invalid public_ssh.host")
	}
	numeric := true
	for _, c := range h {
		if (c < '0' || c > '9') && c != '.' {
			numeric = false
		}
	}
	if numeric {
		return fmt.Errorf("invalid public_ssh.host IPv4 literal")
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid public_ssh.host DNS label")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("invalid public_ssh.host DNS character")
			}
		}
	}
	return nil
}
