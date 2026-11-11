package sshaccess

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCountEstablishedExcludesExactMonitorTuple(t *testing.T) {
	const self = "172.17.0.1 40000 172.17.0.2 22\n"
	cases := []struct {
		name, out string
		want      int
		bad       bool
	}{
		{"only monitor", self + "0 0 172.17.0.2:22 172.17.0.1:40000\n", 0, false},
		{"active plus sftp", self + "0 0 172.17.0.2:22 172.17.0.1:40000\n0 0 172.17.0.2:22 172.17.0.1:40001\n0 0 172.17.0.2:22 10.0.0.9:5000\n", 2, false},
		{"same peer other port counts", self + "0 0 172.17.0.2:22 172.17.0.1:40000\n0 0 172.17.0.2:22 172.17.0.1:40002\n", 1, false},
		{"ipv6", "fd00::1 41000 fd00::2 22\n0 0 [fd00::2]:22 [fd00::1]:41000\n0 0 [fd00::2]:22 [fd00::9]:2222\n", 1, false},
		{"ipv6 zero compression and case", "FD00:0:0::1 41000 fd00::2 22\n0 0 [fd00::2]:22 [fd00::1]:41000\n", 0, false},
		{"ipv4 mapped", "172.17.0.1 40000 172.17.0.2 22\n0 0 [::ffff:172.17.0.2]:22 [::ffff:172.17.0.1]:40000\n0 0 [::ffff:172.17.0.2]:22 [::ffff:10.1.1.1]:1\n", 1, false},
		{"scoped link local", "fe80::1 41000 fe80::2 22\n0 0 [fe80::2%eth0]:22 [fe80::1%eth0]:41000\n0 0 [fe80::2%eth0]:22 [fe80::5%eth0]:7\n", 1, false},
		{"mixed families", self + "0 0 172.17.0.2:22 172.17.0.1:40000\n0 0 [fd00::2]:22 [fd00::7]:1\n", 1, false},
		{"monitor absent", self + "0 0 172.17.0.2:22 10.0.0.9:1\n", 0, true},
		{"monitor duplicated", self + "0 0 172.17.0.2:22 172.17.0.1:40000\n0 0 172.17.0.2:22 172.17.0.1:40000\n", 0, true},
		{"malformed line", self + "0 0 172.17.0.2:22 172.17.0.1:40000\ngarbage\n", 0, true},
		{"bad port", self + "0 0 172.17.0.2:22 172.17.0.1:x\n", 0, true},
		{"not port 22", self + "0 0 172.17.0.2:22 172.17.0.1:40000\n0 0 172.17.0.2:80 10.0.0.1:5\n", 0, true},
		{"empty ssh connection", "\n", 0, true},
		{"short ssh connection", "1.2.3.4 5\n", 0, true},
		{"empty output", "", 0, true},
	}
	for _, c := range cases {
		got, err := CountEstablished(c.out)
		if c.bad {
			if err == nil {
				t.Errorf("%s: expected ambiguity error, got %d", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %d, %v want %d", c.name, got, err, c.want)
		}
	}
}

func TestConnCounterFixedStrictCommand(t *testing.T) {
	d := t.TempDir()
	args := filepath.Join(d, "args")
	bin := filepath.Join(d, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + args + "'\nprintf '172.17.0.1 40000 172.17.0.2 22\\n0 0 172.17.0.2:22 172.17.0.1:40000\\n0 0 172.17.0.2:22 172.17.0.1:40003\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	creds := &shutdownLease{key: filepath.Join(d, "dedicated-key")}
	c := ConnCounter{Credentials: creds, RuntimeDir: d, KnownHostsFile: filepath.Join(d, "known_hosts"), binary: bin}
	n, err := c.Count(context.Background(), "vm", "172.17.0.2", "root")
	if err != nil || n != 1 || !creds.released {
		t.Fatalf("n=%d err=%v released=%v", n, err, creds.released)
	}
	raw, _ := os.ReadFile(args)
	a := string(raw)
	for _, want := range []string{"StrictHostKeyChecking=yes", "UserKnownHostsFile=" + c.KnownHostsFile, "GlobalKnownHostsFile=/dev/null", "IdentityAgent=none", "IdentitiesOnly=yes", "PasswordAuthentication=no", "-F\n/dev/null", "root@172.17.0.2", "SSH_CONNECTION", "ss -H -n -t state established"} {
		if !strings.Contains(a, want) {
			t.Fatalf("missing %q in %s", want, a)
		}
	}
	for _, bad := range []string{"accept-new", "ProxyJump", "ClearAllForwardings=no"} {
		if strings.Contains(a, bad) {
			t.Fatal(a)
		}
	}
}

func TestConnCounterFailsClosed(t *testing.T) {
	d := t.TempDir()
	creds := &shutdownLease{key: "PRIVATE-PATH-MUST-NOT-LEAK"}
	ok := ConnCounter{Credentials: creds, RuntimeDir: d, KnownHostsFile: "/trusted/known_hosts", binary: filepath.Join(d, "missing-ssh")}
	for name, c := range map[string]ConnCounter{
		"no credentials": {RuntimeDir: d, KnownHostsFile: "/k"},
		"no trust":       {Credentials: creds, RuntimeDir: d},
		"ssh failure":    ok,
	} {
		if _, err := c.Count(context.Background(), "vm", "172.17.0.2", "root"); err == nil || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), d) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if _, err := ok.Count(context.Background(), "vm", "not-an-ip", "root"); err == nil {
		t.Error("hostname accepted")
	}
	if _, err := ok.Count(context.Background(), "vm", "172.17.0.2", "-oProxyCommand=x"); err == nil {
		t.Error("unsafe user accepted")
	}
}
