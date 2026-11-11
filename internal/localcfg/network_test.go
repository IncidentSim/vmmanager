package localcfg

import (
	"strings"
	"testing"
)

func TestGuestNetworkProvisioningPolicy(t *testing.T) {
	for _, tc := range []struct {
		fields string
		ok     bool
	}{
		{`"guest_network_provisioning":"ubuntu-fcnet","guest_dns":["1.1.1.1"]`, true},
		{`"guest_network_provisioning":"unknown","guest_dns":["1.1.1.1"]`, false},
		{`"guest_network_provisioning":"ubuntu-fcnet"`, false},
		{`"guest_network_provisioning":"ubuntu-fcnet","guest_dns":["evil; reboot"]`, false},
	} {
		_, err := Parse([]byte(strings.Replace(good, `"max_vms":2`, `"max_vms":2,`+tc.fields, 1)))
		if (err == nil) != tc.ok {
			t.Fatalf("%s err=%v", tc.fields, err)
		}
	}
}
