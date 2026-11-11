package main

import (
	"os"
	"strings"
	"testing"

	"github.com/petruekhin/vibe-firecracker/internal/vmhelper"
)

func TestHandlerUsesRootPoolPublicationOnly(t *testing.T) {
	for _, public := range []bool{false, true} {
		config := "GUEST_PREFIX=10.213\nMAX_SLOTS=4\n"
		if public {
			config += "PUBLIC_IPV4=185.70.184.64\nSSH_PORT_BASE=22000\n"
		}
		pool, err := vmhelper.ParsePool([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		h := handlerForPool(pool)
		if h.PublicEnabled != public || h.Systemctl != "/usr/bin/systemctl" || h.LockDir != "" || h.RunNetworkCommand != nil {
			t.Fatalf("unsafe production handler: %+v", h)
		}
	}
}

func TestHelperSandboxPermitsOnlyRuntimeLocksAndConntrackNetlink(t *testing.T) {
	raw, err := os.ReadFile("../../deployment/multivm/fc-vm-helper@.service")
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			settings[k] = v
		}
	}
	if settings["ReadWritePaths"] != "/run/firecracker-vms /run/firecracker-vms-net" {
		t.Fatalf("unexpected writable scope: %q", settings["ReadWritePaths"])
	}
	if settings["ProtectSystem"] != "strict" {
		t.Fatal("filesystem protection relaxed")
	}
	if settings["RestrictAddressFamilies"] != "AF_UNIX AF_NETLINK" {
		t.Fatal("conntrack needs netlink; no IP sockets needed")
	}
}
