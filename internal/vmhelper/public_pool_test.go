package vmhelper

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual shell check parser against the Go parser with the same
// adversarial corpus. ip is always mocked; no privilege/network access occurs.
func TestShellPoolParserAgreement(t *testing.T) {
	source, err := os.ReadFile("../../deployment/multivm/fc-vm-network.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pool := filepath.Join(dir, "pool")
	ip := filepath.Join(dir, "ip")
	if err := os.WriteFile(ip, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(source), "/etc/firecracker-vms/pool.conf", pool)
	script = strings.ReplaceAll(script, "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "PATH="+dir+":/usr/bin:/bin")
	path := filepath.Join(dir, "net.sh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	base := "GUEST_PREFIX=10.213\nMAX_SLOTS=4\n"
	corpus := []string{base, strings.ReplaceAll(base, "\n", "\r\n"), " " + base, base + "# comment\n", base + " # comment\n", base + "# NUL\x00\n", base + "MAX_SLOTS=4\n", strings.ReplaceAll(base, "=4", "=04")}
	for _, addr := range []string{"8.8.4.4", "1.1.1.1", "0.1.2.3", "10.0.0.1", "127.0.0.1", "169.254.169.254", "100.100.100.200", "172.16.0.1", "192.168.1.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "255.255.255.255", "8.8.4.004", "256.1.1.1", "::1", "$(id)", "", "8.8.4.4 "} {
		for _, port := range []string{"1024", "22000", "65532", "65533", "022000", "0", "65536", "$(id)", ""} {
			corpus = append(corpus, base+"PUBLIC_IPV4="+addr+"\nSSH_PORT_BASE="+port+"\n")
		}
	}
	for _, data := range corpus {
		if err := os.WriteFile(pool, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		_, goErr := ParsePool([]byte(data))
		out, shellErr := exec.Command("bash", path, "check", "0").CombinedOutput()
		if (goErr == nil) != (shellErr == nil) {
			t.Errorf("parser disagreement for %q: Go=%v shell=%v %s", data, goErr, shellErr, out)
		}
	}
}

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
