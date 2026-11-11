//go:build linux

package localcfg

import (
	"strings"
	"testing"
)

func TestGuestNetworkAndPublicSSHAssembled(t *testing.T) {
	cfg, err := Parse([]byte(strings.Replace(good, `"max_vms":2`, `"max_vms":2,"guest_network_provisioning":"ubuntu-fcnet","guest_dns":["1.1.1.1"],"public_ssh":{"host":"203.0.113.10","port_base":22000}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.PoolConf = publicationPool(t, "PUBLIC_IPV4=8.8.8.8\nSSH_PORT_BASE=22000\n")
	cfg.PublicSSH.Host = "8.8.8.8"
	c, err := cfg.Assemble(Seams{Launcher: fakeLauncher{}, VMM: fakeVMM{}})
	if err != nil || c.GuestNetwork == nil || c.PublicSSH == nil || c.PublicSSH.Host != "8.8.8.8" || c.PublicSSH.PortBase != 22000 {
		t.Fatalf("%+v err=%v", c, err)
	}
}
