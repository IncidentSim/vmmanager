package vmhelper

import (
	"testing"
)

func TestPublicPoolOptions(t *testing.T) {
	base := "GUEST_PREFIX=10.213\nMAX_SLOTS=4\n"
	p, err := ParsePool([]byte(base + "PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=22000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.PublicIPv4 != "8.8.4.4" || p.SSHPortBase != 22000 {
		t.Fatalf("wrong public pool: %+v", p)
	}
	for _, extra := range []string{
		"PUBLIC_IPV4=8.8.4.4\n", "SSH_PORT_BASE=22000\n", "PUBLIC_IPV4=008.8.4.4\nSSH_PORT_BASE=22000\n",
		"PUBLIC_IPV4=10.0.0.1\nSSH_PORT_BASE=22000\n", "PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=65534\n",
		"PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=022000\n", "PUBLIC_IPV4=$(id)\nSSH_PORT_BASE=22000\n",
		"PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=22000\nPUBLIC_IPV4=8.8.4.4\n", " PUBLIC_IPV4=8.8.4.4\nSSH_PORT_BASE=22000\n",
	} {
		if _, err := ParsePool([]byte(base + extra)); err == nil {
			t.Errorf("accepted %q", extra)
		}
	}
}
