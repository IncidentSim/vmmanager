package localcfg

import (
	"strings"
	"testing"
)

func TestPublicSSHOperatorPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy string
		ok     bool
	}{
		{`{"host":"203.0.113.10","port_base":22000}`, true},
		{`{"host":"ssh.example.test","port_base":65534}`, true},
		{`{"host":"ssh.example.test","port_base":65535}`, false},
		{`{"host":"ssh.example.test","port_base":0}`, false},
		{`{"host":"ssh.example.test","port_base":-1}`, false},
		{`{"host":"ssh.example.test","port_base":65536}`, false},
		{`{"host":"ssh.example.test","port_base":22,"target_port":23}`, false},
		{`{"host":"","port_base":22000}`, false},
		{`{"host":"-option","port_base":22000}`, false},
		{`{"host":"https://ssh.example.test","port_base":22000}`, false},
		{`{"host":"user@ssh.example.test","port_base":22000}`, false},
		{`{"host":"ssh.example.test:22","port_base":22000}`, false},
		{`{"host":"::1","port_base":22000}`, false},
		{`{"host":"999.1.1.1","port_base":22000}`, false},
		{`{"host":"a..test","port_base":22000}`, false},
		{`{"host":"a-.test","port_base":22000}`, false},
		{`{"host":"has space.test","port_base":22000}`, false},
		{`{"host":"203.0.113.10","port_base":22000,"advertised_host":"rkn.petruekhin.tech"}`, true},
		{`{"host":"203.0.113.10","port_base":22000,"advertised_host":"-bad.test"}`, false},
		{`{"host":"203.0.113.10","port_base":22000,"advertised_host":"u@x.test"}`, false},
		{`{"host":"203.0.113.10","port_base":22000,"advertised_host":"x.test:22"}`, false},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			input := strings.Replace(good, `"max_vms":2`, `"max_vms":2,"public_ssh":`+tc.policy, 1)
			_, err := Parse([]byte(input))
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v want=%v err=%v", err == nil, tc.ok, err)
			}
		})
	}
}
