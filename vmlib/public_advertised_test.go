package vmlib_test

import (
	"testing"

	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func TestPublicSSHAdvertisedHostValidationAndDefault(t *testing.T) {
	cfg := vmlib.PublicSSHConfig{Host: "203.0.113.10", PortBase: 22000}
	if cfg.ReportedHost() != "203.0.113.10" {
		t.Fatal("default must be Host")
	}
	cfg.AdvertisedHost = "rkn.petruekhin.tech"
	if err := cfg.ValidateSlots(4); err != nil || cfg.ReportedHost() != "rkn.petruekhin.tech" {
		t.Fatalf("valid advertised host: %v", err)
	}
	for _, bad := range []string{"-x.test", "a..test", "a b.test", "https://x.test", "u@x.test", "x.test:22", "::1", "999.1.1.1", "a_b.test"} {
		cfg.AdvertisedHost = bad
		if err := cfg.ValidateSlots(4); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
