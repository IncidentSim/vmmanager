package sshaccess

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type shutdownLease struct {
	key      string
	released bool
	calls    int
}

func (s *shutdownLease) TerminalKeyFile(string, string) (string, func(), error) {
	s.calls++
	return s.key, func() { s.released = true }, nil
}
func TestShutdownStrictSSHAndLease(t *testing.T) {
	d := t.TempDir()
	args := filepath.Join(d, "args")
	done := filepath.Join(d, "done")
	bin := filepath.Join(d, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + args + "'\nprintf done > '" + done + "'\nexit 255\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	creds := &shutdownLease{key: filepath.Join(d, "dedicated-key")}
	p := SSHShutdown{Credentials: creds, RuntimeDir: d, KnownHostsFile: filepath.Join(d, "known_hosts"), binary: bin}
	err := p.Reboot(context.Background(), "vm", "172.30.0.2", "root")
	if err == nil || strings.Contains(err.Error(), d) || !creds.released {
		t.Fatalf("unsafe error or lease: %v", err)
	}
	raw, e := os.ReadFile(args)
	if e != nil {
		t.Fatal(e)
	}
	a := string(raw)
	for _, want := range []string{"StrictHostKeyChecking=yes", "UserKnownHostsFile=" + p.KnownHostsFile, "GlobalKnownHostsFile=/dev/null", "IdentityAgent=none", "IdentitiesOnly=yes", "PasswordAuthentication=no", "-F\n/dev/null", "systemctl reboot", "root@172.30.0.2"} {
		if !strings.Contains(a, want) {
			t.Fatalf("missing %s in %s", want, a)
		}
	}
	for _, bad := range []string{"accept-new", "reboot -f", "ProxyJump"} {
		if strings.Contains(a, bad) {
			t.Fatal(a)
		}
	}
	if _, e := os.Stat(done); e != nil {
		t.Fatal("cleanup before process completed")
	}
}
func TestShutdownMissingBinaryDoesNotLeakLeaseOrErrors(t *testing.T) {
	d := t.TempDir()
	creds := &shutdownLease{key: "PRIVATE-PATH-MUST-NOT-LEAK"}
	p := SSHShutdown{Credentials: creds, RuntimeDir: d, KnownHostsFile: "/trusted/known_hosts", binary: filepath.Join(d, "missing-ssh")}
	err := p.Reboot(context.Background(), "vm", "172.30.0.2", "root")
	if err == nil || !creds.released || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), d) {
		t.Fatalf("unsafe missing binary: %v", err)
	}
}

func TestShutdownBoundsLocalProcessAndRejectsTargetInjection(t *testing.T) {
	d := t.TempDir()
	bin := filepath.Join(d, "ssh")
	// exec ensures no descendant retains the lease or output descriptors.
	os.WriteFile(bin, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700)
	creds := &shutdownLease{key: "test-key"}
	p := SSHShutdown{Credentials: creds, RuntimeDir: d, KnownHostsFile: "/trusted/known_hosts", binary: bin}
	for _, host := range []string{"-oProxyCommand=bad", "host;bad", "127.0.0.1 x"} {
		if p.Reboot(context.Background(), "vm", host, "root") == nil {
			t.Fatal("unsafe target accepted")
		}
	}
	if creds.calls != 0 {
		t.Fatal("unsafe target leased key")
	}
	c, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if p.Reboot(c, "vm", "172.30.0.2", "root") == nil || !creds.released || time.Since(started) > time.Second {
		t.Fatal("unbounded shutdown or leaked lease")
	}
}
